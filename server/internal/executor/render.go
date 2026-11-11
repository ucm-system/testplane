// Package executor 把一份 ports.JobSpec（spec 2.0）渲染成 batch/v1 Job，并负责
// 提交、观测、取消、日志读取与回收（design/impl/execution.md §2–§7）。
//
// v2 契约（D-66～D-72）：Job 名 `argus-<task>-<attempt>-<seq>`；`command` =
// runner.yaml.entry（exec form 数组）、`args` 恒空（业务输入走输入文件 +
// AG_PARAMS_FILE）；env 全 AG_*；挂载三种（input 只读 / output 读写 /
// profile_mount）；标签 `argus.io/*`；非 root / 只读根 fs；不绑 ServiceAccount；
// **绝不设置 ownerReferences**（D-72：runner 进程生命周期 ≠ 资源使用生命周期）。
//
// 边界（execution §1）：执行器只产出执行事实；不判断业务状态、不写状态表、
// 不解析参数语义。所有状态推进由编排层在拿到 JobObserver 回调后完成。
package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// 平台固定渲染项（execution §2.1 的"固定渲染项"表）与命名常量。
// v2 命名（D-66）：标签统一 `argus.io/*`，取代 v1 的 `testplane.io/*`。
const (
	// LabelApp 是粗粒度筛选标签（人肉 kubectl 用）。
	LabelApp = "app"
	// LabelManaged 是执行层所有 K8s 对象的统一标记，GC 与 list/watch 都靠它（§6.3）。
	LabelManaged = "argus.io/managed"
	// LabelJobID 是幂等键 j-XXXXXXXX，Pod 侧二级过滤也用它（§4.1）。
	LabelJobID = "argus.io/job-id"
	// LabelTask / LabelAttempt / LabelRunnerSeq / LabelRole 是必带 label（§2.3，D-66）。
	LabelTask      = "argus.io/task"
	LabelAttempt   = "argus.io/attempt"
	LabelRunnerSeq = "argus.io/runner-seq"
	LabelRole      = "argus.io/role"

	// 必带 annotation（§2.3）。取消后补的 canceled-by/canceled-at 见 Cancel。
	AnnSpecHash       = "argus.io/spec-hash"
	AnnRunnerVersion  = "argus.io/runner-version"
	AnnTimeoutSeconds = "argus.io/timeout-seconds"
	AnnRequestedBy    = "argus.io/requested-by"
	AnnCanceledAt     = "argus.io/canceled-at"
	AnnCanceledBy     = "argus.io/canceled-by"
	// AnnRunnerKey 是 Pod 模板上的三元组快照（task:attempt:seq，execution §2.2）。
	AnnRunnerKey = "argus.io/runner-key"
)

// 平台保留的容器内路径与默认值。
const (
	// RunnerContainerName 是唯一容器名（execution §2.2）。
	RunnerContainerName = "runner"
	// OutputMountPath 是可写产物挂载点，其余路径只读（§3.3）。
	// 平台保留挂载点统一来自 domain（#74：registry 与 render 曾有两套不一致的集合）。
	OutputMountPath = domain.WorkspaceOutputPath
	// InputMountPath 是输入只读挂载点（该 runner 的 input/<seq>/ 子目录，E-Q8）。
	InputMountPath = domain.WorkspaceInputPath
	// TmpMountPath 配合只读根：/tmp 必须是可写 emptyDir（§2.1、§3.3）。
	TmpMountPath = domain.TmpPath
	// ArgusUID / ArgusGID 是默认运行身份（§2.2、§3.5；E-Q5）。
	ArgusUID int64 = 10001
	ArgusGID int64 = 10001
	// tmpSizeLimit 是 /tmp emptyDir 的上限。
	tmpSizeLimit = "256Mi"
	// deadlineGraceSeconds 是 activeDeadlineSeconds 相对平台超时的缓冲（§5.1）。
	deadlineGraceSeconds = 60
	// maxJobNameLen 是 DNS-1123 label 上限。
	maxJobNameLen = 63
	// maxTimeoutSeconds 是渲染期允许的最大超时（§12.7）。
	maxTimeoutSeconds = 86400
	// DefaultTZ 是强制注入的时区（§2.2）。
	DefaultTZ = "Asia/Shanghai"
	// DefaultStoragePVC 是产物 PVC 名（D-41：testplane-storage → argus-storage）。
	DefaultStoragePVC = "argus-storage"
	// DefaultNamespace 是 Job 所在 namespace 的缺省值。
	DefaultNamespace = "argus"

	// 强制注入/覆写的环境变量（execution §2.1：AG_* 元信息 + TZ + HOME=/tmp）。
	EnvTaskID     = "AG_TASK_ID"
	EnvAttempt    = "AG_ATTEMPT"
	EnvRunnerSeq  = "AG_RUNNER_SEQ"
	EnvParamsFile = "AG_PARAMS_FILE"
	EnvOutputDir  = "AG_OUTPUT_DIR"
	EnvTZ         = "TZ"
	EnvHome       = "HOME"
	// defaultParamsFile 是 AG_PARAMS_FILE 的缺省值（jsonl 形态；yaml 形态由
	// 编排层经 spec.Env 覆盖——执行层不感知 input_format，D-67）。
	defaultParamsFile = "/workspace/input/params.jsonl"
	// defaultLatestTag 表示无 tag/`:latest` 时用 Always。
	defaultLatestTag = "latest"
)

// 渲染期错误。这些错误都在创建 Job 之前暴露（§3.4），不可重试，映射到平台侧
// 校验错误（VALIDATION_ERROR / MOUNT_UNAVAILABLE / CONFLICT）。
var (
	// ErrSpecInvalid 表示 JobSpec 字段缺失或不合法（不可重试）。
	ErrSpecInvalid = errors.New("executor: invalid job spec")
	// ErrBadSpecVersion 表示 JobSpec 的 spec_version 不是 2.0（附录 B：未知版本必须拒绝）。
	ErrBadSpecVersion = errors.New("executor: unsupported job spec version")
	// ErrBadPath 表示产物路径无法换算成合法的 PVC subPath（§3.1）。
	ErrBadPath = errors.New("executor: invalid storage path")
	// ErrSpecConflict 表示同名 Job 已存在但 spec-hash 不同（§2.3 的 CONFLICT）。
	ErrSpecConflict = errors.New("executor: job already exists with a different spec")
	// ErrJobNotFound 表示 Job 与 Pod 都已消失。
	ErrJobNotFound = errors.New("executor: job not found")
	// ErrCleanupIncomplete 表示删除后仍有 Pod 残留（§5.2 第 4 步，上游记 CLEANUP_FAILED）。
	ErrCleanupIncomplete = errors.New("executor: pods still present after cancel")
)

// segmentPattern 约束 subPath 的每一段（§3.1 的 attempts/<n>/...）。
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// secretRefPattern 把 SecretEnvRef 里的 "secretName/key"（或 "secretName:key"）拆开。
// 引用名不参与推导（execution §2.1）：解析不出就跳过并告警，不猜 Secret 名。
var secretRefPattern = regexp.MustCompile(`^([^/:=]+)[/:](.+)$`)

// Render 把 JobSpec 渲染成 batch/v1 Job。纯函数：不碰集群、无副作用。
func Render(spec ports.JobSpec) (*batchv1.Job, error) {
	job, _, err := RenderWithWarnings(spec)
	return job, err
}

// RenderWithWarnings 与 Render 相同，但额外返回不影响执行的告警（execution §4.3.1 的
// "只记 warning，不重试、不影响状态"口径）：
//
//   - `secret_env_unbound`：secret_env 只声明了变量名、没有映射到 Secret 名与 key。
//     执行层**跳过**该变量并告警，不 fail——"required 且完全没绑定"属于提交期校验
//     （C-5.1）的责任，执行器不承担。
func RenderWithWarnings(spec ports.JobSpec) (*batchv1.Job, []string, error) {
	if err := validateSpec(spec); err != nil {
		return nil, nil, err
	}
	namespace := spec.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	outputSub, err := OutputSubPath(spec.TaskID, spec.AttemptNo, spec.Seq)
	if err != nil {
		return nil, nil, err
	}
	if err := checkStorageDir(spec.OutputDir, outputSub); err != nil {
		return nil, nil, err
	}
	inputSub := ""
	if spec.InputDir != "" {
		inputSub, err = InputSubPath(spec.TaskID, spec.AttemptNo, spec.Seq)
		if err != nil {
			return nil, nil, err
		}
		if err := checkStorageDir(spec.InputDir, inputSub); err != nil {
			return nil, nil, err
		}
	}

	jobID := spec.JobID
	if jobID == "" {
		jobID = JobID(spec.TaskID, spec.AttemptNo, spec.Seq)
	}

	labels, err := buildLabels(spec, jobID)
	if err != nil {
		return nil, nil, err
	}
	annotations, err := buildAnnotations(spec)
	if err != nil {
		return nil, nil, err
	}

	env, warnings, err := buildEnv(spec)
	if err != nil {
		return nil, nil, err
	}
	res, err := buildResources(spec.Resources)
	if err != nil {
		return nil, nil, err
	}
	mounts, volumes, err := buildMounts(spec, inputSub, outputSub)
	if err != nil {
		return nil, nil, err
	}

	podLabels := map[string]string{
		LabelApp:     "argus",
		LabelManaged: "true",
		LabelJobID:   jobID,
	}
	podAnnotations := map[string]string{
		AnnRunnerKey: runnerKey(spec.TaskID, spec.AttemptNo, spec.Seq),
	}
	for k, v := range spec.Annotations {
		podAnnotations[k] = v
	}

	timeout := int64(spec.TimeoutSeconds) + deadlineGraceSeconds
	backoffLimit := int32(0)
	completions := int32(1)
	parallelism := int32(1)
	gracePeriod := int64(30)
	automount := false // D-72 / §5.10：runner 一律不绑 ServiceAccount、不注入 token
	enableServiceLinks := false
	allowPrivEscalation := false
	readOnlyRoot := true
	runAsNonRoot := true
	runAsUser := ArgusUID
	runAsGroup := ArgusGID
	fsGroup := ArgusGID
	seccomp := corev1.SeccompProfileTypeRuntimeDefault

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        JobName(spec),
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			Completions:           &completions,
			Parallelism:           &parallelism,
			ActiveDeadlineSeconds: &timeout,
			// 不设 ttlSecondsAfterFinished（§2.4：平台要读 containerStatuses，
			// TTL 会抢在日志 follower 收尾前删 Pod）、不设 Selector、**不设 OwnerReferences**
			// （D-72：控制面不是 K8s 对象；级联删除靠 label 扫描 + 平台 GC）。
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					AutomountServiceAccountToken:  &automount,
					EnableServiceLinks:            &enableServiceLinks,
					TerminationGracePeriodSeconds: &gracePeriod,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &runAsNonRoot,
						RunAsUser:      &runAsUser,
						RunAsGroup:     &runAsGroup,
						FSGroup:        &fsGroup,
						SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
					},
					Containers: []corev1.Container{{
						Name:            RunnerContainerName,
						Image:           spec.Image,
						ImagePullPolicy: imagePullPolicy(spec.Image),
						// command = runner.yaml.entry（exec form 数组，逐元素透传）；
						// args **恒不渲染**（D-66：业务输入走输入文件，平台不追加 argv）。
						Command:   append([]string(nil), spec.Command...),
						Env:       env,
						Resources: res,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivEscalation,
							ReadOnlyRootFilesystem:   &readOnlyRoot,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}

	hash, err := SpecHash(job)
	if err != nil {
		return nil, nil, err
	}
	job.Annotations[AnnSpecHash] = hash
	return job, warnings, nil
}

// SpecHash 计算用于幂等比对的指纹（§2.3）。指纹覆盖 Job 的名称、label、
// 除 spec-hash 之外的 annotation，以及完整 spec；不含 status 与时间戳。
func SpecHash(job *batchv1.Job) (string, error) {
	type fingerprint struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        batchv1.JobSpec   `json:"spec"`
	}
	ann := make(map[string]string, len(job.Annotations))
	for k, v := range job.Annotations {
		if k == AnnSpecHash {
			continue
		}
		ann[k] = v
	}
	fp := fingerprint{
		Name:        job.Name,
		Labels:      job.Labels,
		Annotations: ann,
		Spec:        job.Spec,
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return "", fmt.Errorf("executor: hash job spec: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateSpec(spec ports.JobSpec) error {
	switch {
	case spec.SpecVersion != ports.JobSpecVersion2:
		return fmt.Errorf("%w: spec_version %q, want %q", ErrBadSpecVersion, spec.SpecVersion, ports.JobSpecVersion2)
	case spec.TaskID == "":
		return fmt.Errorf("%w: task_id is empty", ErrSpecInvalid)
	case spec.AttemptNo < 1:
		return fmt.Errorf("%w: attempt_no must be >= 1, got %d", ErrSpecInvalid, spec.AttemptNo)
	case spec.Seq < 1:
		return fmt.Errorf("%w: seq must be >= 1, got %d", ErrSpecInvalid, spec.Seq)
	case spec.Image == "":
		return fmt.Errorf("%w: image is empty", ErrSpecInvalid)
	case len(spec.Command) == 0:
		// D-66：command = runner.yaml.entry（exec form 数组），平台不追加 argv。
		return fmt.Errorf("%w: command (runner entry) is empty", ErrSpecInvalid)
	case spec.Role == "" || !domain.ValidRole(spec.Role):
		return fmt.Errorf("%w: role %q must be one of test|pre_check|deploy|clean", ErrSpecInvalid, spec.Role)
	case spec.TimeoutSeconds <= 0 || spec.TimeoutSeconds > maxTimeoutSeconds:
		return fmt.Errorf("%w: timeout_seconds %d out of (0, %d]", ErrSpecInvalid, spec.TimeoutSeconds, maxTimeoutSeconds)
	case spec.OutputDir == "":
		return fmt.Errorf("%w: output_dir is required (§6.8: 输出就是最终落盘位置)", ErrSpecInvalid)
	}
	if spec.StoragePVC == "" {
		// D-41：PVC 名统一 argus-storage；spec 未给时用缺省值。
		return nil
	}
	return nil
}

// runnerKey 返回 "task:attempt:seq" 三元组字符串（Pod 模板 annotation 用）。
func runnerKey(taskID string, attemptNo, seq int) string {
	return taskID + ":" + strconv.Itoa(attemptNo) + ":" + strconv.Itoa(seq)
}

// OutputSubPath 由三元组直接推导 output subPath（§3.1）：
// attempts/<n>/runners/<seq>。不解析宿主路径，避免"尽力纠正"出跨 Task 目录。
func OutputSubPath(taskID string, attemptNo, seq int) (string, error) {
	return subPathFor(taskID, attemptNo, seq, "runners")
}

// InputSubPath 推导 input subPath：attempts/<n>/input/<seq>（E-Q8：每 runner 一个子目录）。
func InputSubPath(taskID string, attemptNo, seq int) (string, error) {
	return subPathFor(taskID, attemptNo, seq, "input")
}

func subPathFor(taskID string, attemptNo, seq int, kind string) (string, error) {
	parts := []string{"attempts", strconv.Itoa(attemptNo), kind, strconv.Itoa(seq)}
	for _, p := range parts {
		if !segmentPattern.MatchString(p) || strings.Contains(p, "..") {
			return "", fmt.Errorf("%w: bad segment %q", ErrBadPath, p)
		}
	}
	// task 段：平台生成的 task_id 必须路径安全（§6.8 第 1 条：禁止 /、..、空白、
	// 控制字符）。**不做"尽力纠正"**——清洗后的名字会指向另一个 Task 的目录。
	task := strings.TrimSpace(taskID)
	if task == "" || strings.ContainsAny(task, "/\\ \t\n\r") ||
		strings.Contains(task, "..") || !segmentPattern.MatchString(task) {
		return "", fmt.Errorf("%w: bad task segment %q", ErrBadPath, taskID)
	}
	return path.Join(append([]string{task}, parts...)...), nil
}

// checkStorageDir 校验调用方给的宿主路径与推导出的 subPath 一致（§3.1 的"越界即拒绝"）。
// 宿主路径 = <storage-root>/ + subPath；storage-root 对执行器不可见，只校验尾部对齐。
func checkStorageDir(dir, subPath string) error {
	clean := path.Clean(strings.ReplaceAll(dir, "\\", "/"))
	if strings.HasPrefix(clean, "..") || path.IsAbs(clean) && strings.Contains(clean, "..") {
		return fmt.Errorf("%w: %q escapes storage root", ErrBadPath, dir)
	}
	if clean != subPath && !strings.HasSuffix(clean, "/"+subPath) {
		return fmt.Errorf("%w: %q does not end with %q", ErrBadPath, dir, subPath)
	}
	return nil
}

// imagePullPolicy 按 §2.4：digest 或非 latest tag → IfNotPresent；latest/无 tag → Always。
func imagePullPolicy(image string) corev1.PullPolicy {
	if strings.Contains(image, "@sha256:") {
		return corev1.PullIfNotPresent
	}
	lastSlash := strings.LastIndex(image, "/")
	tag := ""
	if i := strings.LastIndex(image, ":"); i > lastSlash {
		tag = image[i+1:]
	}
	if tag == "" || tag == defaultLatestTag {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func buildLabels(spec ports.JobSpec, jobID string) (map[string]string, error) {
	labels := map[string]string{
		LabelApp:     "argus",
		LabelManaged: "true",
		LabelJobID:   jobID,
	}
	set := func(k, v string) error {
		if err := checkLabelValue(v); err != nil {
			return fmt.Errorf("%w: label %s: %v", ErrSpecInvalid, k, err)
		}
		labels[k] = v
		return nil
	}
	for key, val := range map[string]string{
		LabelTask:      spec.TaskID,
		LabelAttempt:   strconv.Itoa(spec.AttemptNo),
		LabelRunnerSeq: strconv.Itoa(spec.Seq),
		LabelRole:      spec.Role,
	} {
		if err := set(key, val); err != nil {
			return nil, err
		}
	}
	if spec.RunnerName != "" {
		if err := set("argus.io/runner", spec.RunnerName); err != nil {
			return nil, err
		}
	}
	// spec.Labels 允许带补充标签；平台派生的键不可被覆盖（§6.8 第 8 条）。
	for k, v := range spec.Labels {
		if _, reserved := labels[k]; reserved {
			continue
		}
		if err := set(k, v); err != nil {
			return nil, err
		}
	}
	return labels, nil
}

func buildAnnotations(spec ports.JobSpec) (map[string]string, error) {
	ann := map[string]string{
		AnnTimeoutSeconds: strconv.Itoa(spec.TimeoutSeconds),
		AnnRequestedBy:    "system:orchestrator",
	}
	if spec.RunnerVersion != "" {
		ann[AnnRunnerVersion] = spec.RunnerVersion
	}
	for k, v := range spec.Annotations {
		ann[k] = v
	}
	return ann, nil
}

// buildEnv 渲染容器环境变量（execution §2.1）：AG_* 元信息 + TZ + HOME=/tmp 强制
// 注入/覆写；spec.Env（编排层传入，含 AG_PARAMS_FILE / AG_SERVED_* 公共参数）按
// key 字典序合并；Secret 变量最后追加 valueFrom.secretKeyRef（凭据不落明文，§12.4）。
//
// 返回的 warnings 是"不影响执行、但要让上游看见"的条目（secret_env_unbound）。
func buildEnv(spec ports.JobSpec) ([]corev1.EnvVar, []string, error) {
	merged := map[string]string{}
	for k, v := range spec.Env {
		merged[k] = v
	}
	// 强制覆写：runner 不得改写平台注入的运行元信息（D-66 的 AG_* 命名）。
	merged[EnvTaskID] = spec.TaskID
	merged[EnvAttempt] = strconv.Itoa(spec.AttemptNo)
	merged[EnvRunnerSeq] = strconv.Itoa(spec.Seq)
	merged[EnvOutputDir] = OutputMountPath
	merged[EnvTZ] = DefaultTZ
	merged[EnvHome] = TmpMountPath
	if _, ok := merged[EnvParamsFile]; !ok || strings.TrimSpace(merged[EnvParamsFile]) == "" {
		merged[EnvParamsFile] = defaultParamsFile
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]corev1.EnvVar, 0, len(keys)+len(spec.SecretEnv))
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: k, Value: merged[k]})
	}

	warnings := make([]string, 0, len(spec.SecretEnv))
	for _, ref := range spec.SecretEnv {
		name := strings.TrimSpace(ref.Name)
		refStr := strings.TrimSpace(ref.SecretRef)
		if name == "" && refStr == "" {
			// 完全未提供的占位项：不是告警，安静跳过。
			continue
		}
		secretName, key, ok := parseSecretRef(refStr)
		if name == "" || !ok {
			// 未绑定（引用为空）或形态不认识：跳过 + 告警，绝不 fail 执行记录。
			warnings = append(warnings, fmt.Sprintf("secret_env_unbound: %s (skipped)", nameOrRaw(name, refStr)))
			continue
		}
		env = append(env, corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  key,
				},
			},
		})
	}
	return env, warnings, nil
}

func nameOrRaw(name, ref string) string {
	if name != "" {
		return name
	}
	return ref
}

// parseSecretRef 解析 "secretName/key"（或 "secretName:key"）——§12.4 的引用形态。
// 引用为空返回 ok=false：调用方跳过并告警，不猜 Secret 名与 key。
func parseSecretRef(ref string) (secretName, key string, ok bool) {
	m := secretRefPattern.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func buildResources(req domain.ResourceRequest) (corev1.ResourceRequirements, error) {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	if req.CPU != "" {
		q, err := resource.ParseQuantity(req.CPU)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("%w: cpu %q: %v", ErrSpecInvalid, req.CPU, err)
		}
		requests[corev1.ResourceCPU] = q
		limits[corev1.ResourceCPU] = q
	}
	if req.Memory != "" {
		q, err := resource.ParseQuantity(req.Memory)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("%w: memory %q: %v", ErrSpecInvalid, req.Memory, err)
		}
		requests[corev1.ResourceMemory] = q
		limits[corev1.ResourceMemory] = q
	}
	if req.GPU.Count < 0 {
		return corev1.ResourceRequirements{}, fmt.Errorf("%w: gpu.count must be >= 0", ErrSpecInvalid)
	}
	if req.GPU.Count > 0 {
		// gpu.count: 0 不渲染（附录 B）；扩展资源必须 requests == limits（§2.1）。
		name := corev1.ResourceName(req.GPU.Resource)
		if name == "" {
			name = corev1.ResourceName("nvidia.com/gpu")
		}
		q := *resource.NewQuantity(int64(req.GPU.Count), resource.DecimalSI)
		requests[name] = q
		limits[name] = q
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}, nil
}

// buildMounts 渲染三种挂载（附录 B v2 / §3.1）：
//   - output：读写，subPath = attempts/<n>/runners/<seq>（就是最终落盘位置，D-16）；
//   - input：只读，subPath = attempts/<n>/input/<seq>（InputDir 为空则不渲染）；
//   - profile_mount：Runner 声明的额外挂载（domain.MountSpec），PVC source；
//   - shared mounts（D-63）：平台级统一共享挂载，Profile 关闭时不注入。
//
// 全部用 PVC + subPath，绝不把 <storage-root> 整体挂进容器（§6.8 第 2 条）。
func buildMounts(spec ports.JobSpec, inputSub, outputSub string) ([]corev1.VolumeMount, []corev1.Volume, error) {
	pvc := spec.StoragePVC
	if pvc == "" {
		pvc = DefaultStoragePVC
	}
	mounts := []corev1.VolumeMount{
		{Name: "output", MountPath: OutputMountPath, SubPath: outputSub},
		{Name: "tmp", MountPath: TmpMountPath},
	}
	volumes := []corev1.Volume{
		{
			Name: "output",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc,
					ReadOnly:  spec.StorageRO,
				},
			},
		},
		{
			Name: "tmp",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourcePtr(tmpSizeLimit)},
			},
		},
	}

	if inputSub != "" {
		// 同一 claim **复用 output 的 volume 声明**（一条卷目、多个挂载，§6.8）：
		// 两条同 claim 卷目会让 kubelet 对同一 hostPath 目录做第二次 bind，
		// 在 Docker Desktop 的 drvfs 上实测悬挂（kind/Windows），Pod 永远
		// ContainerCreating。只读差异在 mount 级（ReadOnly: true）表达。
		mounts = append(mounts, corev1.VolumeMount{
			Name: "output", MountPath: InputMountPath, SubPath: inputSub, ReadOnly: true,
		})
	}

	// volume 名去重：`sanitizeLabel` 之后不同 name 可能撞名（K8s 会以
	// "Duplicate value" 拒绝整份 Pod）。这里统一分配唯一名。
	volNames := map[string]int{"output": 0, "tmp": 0, "input": 0}
	uniqueVolumeName := func(base string) string {
		name := sanitizeLabel(base)
		if name == "" {
			name = "mnt"
		}
		if n := volNames[name]; n > 0 {
			volNames[name] = n + 1
			return fmt.Sprintf("%s-%d", name, n+1)
		}
		volNames[name] = 1
		return name
	}

	// 统一共享挂载（D-63）：平台级注入，Profile 关闭时不注入。
	// 用 PVC source（内联 nfs 会被 restricted PSA 拒绝），平台不理解 NFS 协议（§10.4）。
	shared := spec.SharedMounts
	if spec.SharedMountsDisabled {
		shared = nil
	}
	for i, sm := range shared {
		if sm.MountPath == "" || !path.IsAbs(sm.MountPath) {
			return nil, nil, fmt.Errorf("%w: sharedMounts[%d].mountPath must be absolute", ErrSpecInvalid, i)
		}
		if sm.ClaimName == "" {
			return nil, nil, fmt.Errorf("%w: sharedMounts[%d].claimName is empty", ErrSpecInvalid, i)
		}
		name := uniqueVolumeName(sm.Name)
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: sm.MountPath, ReadOnly: sm.ReadOnly})
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: sm.ClaimName,
					ReadOnly:  sm.ReadOnly,
				},
			},
		})
	}

	used := domain.ReservedMountPaths(shared)
	for i, m := range spec.Mounts {
		if m.MountPath == "" {
			return nil, nil, fmt.Errorf("%w: mounts[%d].mount_path is empty", ErrSpecInvalid, i)
		}
		if !path.IsAbs(m.MountPath) {
			return nil, nil, fmt.Errorf("%w: mounts[%d].mount_path must be absolute", ErrSpecInvalid, i)
		}
		if _, dup := used[m.MountPath]; dup {
			return nil, nil, fmt.Errorf("%w: mounts[%d].mount_path %q conflicts with a platform mount",
				ErrSpecInvalid, i, m.MountPath)
		}
		used[m.MountPath] = "runner mount"
		name := m.Name
		if name == "" {
			name = fmt.Sprintf("mnt-%d", i)
		}
		name = uniqueVolumeName(name)
		claim := m.Source
		if claim == "" {
			claim = pvc
		}
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim,
					ReadOnly:  m.ReadOnly,
				},
			},
		})
	}
	return mounts, volumes, nil
}

func resourcePtr(q string) *resource.Quantity {
	v := resource.MustParse(q)
	return &v
}

// JobName 生成 DNS-1123 合法的 Job 名（§2.3）：`argus-<task_id>-<attempt>-<seq>`，
// 超长时截断 task 段并追加内容指纹后缀，保证稳定可重放（同一三元组永远同名）。
func JobName(spec ports.JobSpec) string {
	return Naming(spec.TaskID, spec.AttemptNo, spec.Seq)
}

// Naming 是 JobName 的显式形态，便于在别处（GC、日志、编排层）复用同一规则。
func Naming(taskID string, attemptNo, seq int) string {
	task := sanitizeLabel(taskID)
	if task == "" {
		task = "task"
	}
	suffix := "-" + strconv.Itoa(attemptNo) + "-" + strconv.Itoa(seq)
	name := "argus-" + task + suffix
	if len(name) <= maxJobNameLen {
		return name
	}
	digest := sha256.Sum256([]byte(taskID + "/" + strconv.Itoa(attemptNo) + "/" + strconv.Itoa(seq)))
	tail := suffix + "-" + hex.EncodeToString(digest[:])[:8]
	keep := maxJobNameLen - len("argus-") - len(tail)
	if keep < 1 {
		keep = 1
	}
	if keep > len(task) {
		keep = len(task)
	}
	trimmed := strings.Trim(task[:keep], "-")
	if trimmed == "" {
		trimmed = "task"
	}
	name = "argus-" + trimmed + tail
	if len(name) > maxJobNameLen {
		name = name[:maxJobNameLen]
	}
	return strings.TrimRight(name, "-")
}

// JobID 是与 Job 名同源派生的不透明幂等键（§2.3）：j- + sha256(task/attempt/seq)[:8]。
// 幂等键 = (task_id, attempt_no, seq) 的派生（附录 B / D-68）。
func JobID(taskID string, attemptNo, seq int) string {
	sum := sha256.Sum256([]byte(taskID + "/" + strconv.Itoa(attemptNo) + "/" + strconv.Itoa(seq)))
	return "j-" + hex.EncodeToString(sum[:])[:8]
}

// sanitizeLabel 把任意字符串变成 DNS-1123 label 允许的形态。
func sanitizeLabel(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	var b strings.Builder
	lastDash := false
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.' || r == '/':
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// checkLabelValue 校验 label 值形态（§2.3）。
func checkLabelValue(v string) error {
	if v == "" {
		return nil
	}
	if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
