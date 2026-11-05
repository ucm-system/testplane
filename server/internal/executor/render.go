// Package executor 把一份 ports.JobSpec 渲染成 batch/v1 Job，并负责提交、观测、
// 取消与日志读取（design/impl/execution.md §2–§7）。
//
// 边界（execution §1）：执行器只产出执行事实；不判断业务状态、不写状态表、
// 不解析参数语义。所有状态推进由 orchestrator 在拿到 JobObserver 回调后完成。
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
const (
	// LabelApp 是粗粒度筛选标签（人肉 kubectl 用）。
	LabelApp = "app"
	// LabelManaged 是执行层所有 K8s 对象的统一标记，GC 与 list/watch 都靠它（§6.3）。
	LabelManaged = "testplane.io/managed"
	// LabelJobID 是幂等键 j-XXXXXXXX，Pod 侧二级过滤也用它（§4.1）。
	LabelJobID = "testplane.io/job-id"
	// LabelRun / LabelTask / LabelAttempt / LabelTester / LabelRole 是必带 label（§2.3）。
	LabelRun     = "testplane.io/run"
	LabelTask    = "testplane.io/task"
	LabelAttempt = "testplane.io/attempt"
	LabelTester  = "testplane.io/tester"
	LabelRole    = "testplane.io/role"

	// 必带 annotation（§2.3）。取消后补的 canceled-by/reason 见 Cancel。
	AnnSpecHash       = "testplane.io/spec-hash"
	AnnTesterVersion  = "testplane.io/tester-version"
	AnnTimeoutSeconds = "testplane.io/timeout-seconds"
	AnnRequestedBy    = "testplane.io/requested-by"
	AnnCanceledAt     = "testplane.io/cancel-requested-at"
	AnnCanceledBy     = "testplane.io/canceled-by"
)

// 平台保留的容器内路径与默认值。
const (
	// TesterContainerName 是唯一容器名（execution §2.2）。
	TesterContainerName = "tester"
	// OutputMountPath 是可写产物挂载点，其余路径只读（§3.3）。
	OutputMountPath = "/workspace/output"
	// InputMountPath 是可选输入只读挂载点。
	InputMountPath = "/workspace/input"
	// TmpMountPath 配合只读根：/tmp 必须是可写 emptyDir（§2.1、§3.3）。
	TmpMountPath = "/tmp"
	// DefaultEntry 是 Tester 契约规定的容器内入口（§10 校验项 4）。
	DefaultEntry = "/tester/run.sh"
	// TestplaneUID / TestplaneGID 是默认运行身份（§2.2、§3.5）。
	TestplaneUID int64 = 10001
	TestplaneGID int64 = 10001
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
	// TestplanEnvRunID / TaskID / AttemptID 是强制注入的运行元信息（§2.2）。
	TestplaneEnvRunID     = "TESTPLANE_RUN_ID"
	TestplaneEnvTaskID    = "TESTPLANE_TASK_ID"
	TestplaneEnvAttemptID = "TESTPLANE_ATTEMPT_ID"
	TestplaneEnvTZ        = "TZ"
	TestplaneEnvHome      = "HOME"
	// DefaultImagePullPolicyTag 表示无 tag/`:latest` 时用 Always。
	defaultLatestTag = "latest"
)

// 渲染期错误。这些错误都在创建 Job 之前暴露（§3.4），不可重试，映射到平台侧
// 校验错误（VALIDATION_ERROR / MOUNT_UNAVAILABLE / CONFLICT）。
var (
	// ErrSpecInvalid 表示 JobSpec 字段缺失或不合法（不可重试）。
	ErrSpecInvalid = errors.New("executor: invalid job spec")
	// ErrBadPath 表示产物路径无法换算成合法的 PVC subPath（§3.1）。
	ErrBadPath = errors.New("executor: invalid storage path")
	// ErrSpecConflict 表示同名 Job 已存在但 spec-hash 不同（§2.3 的 CONFLICT）。
	ErrSpecConflict = errors.New("executor: job already exists with a different spec")
	// ErrJobNotFound 表示 Job 与 Pod 都已消失。
	ErrJobNotFound = errors.New("executor: job not found")
	// ErrCleanupIncomplete 表示删除后仍有 Pod 残留（§5.2 第 4 步，上游记 CLEANUP_FAILED）。
	ErrCleanupIncomplete = errors.New("executor: pods still present after cancel")
)

// segmentPattern 约束 subPath 的每一段（§3.1 的 ^<run>/tasks/<task>/attempts/<n>$）。
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// secretEnvPattern 把 SecretEnv 里的 "ENV=secretName/key" 拆开。
var secretEnvPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=([^/:=]+)[/:](.+)$`)

// Render 把 JobSpec 渲染成 batch/v1 Job。纯函数：不碰集群、无副作用。
func Render(spec ports.JobSpec) (*batchv1.Job, error) {
	job, _, err := RenderWithWarnings(spec)
	return job, err
}

// RenderWithWarnings 与 Render 相同，但额外返回不影响执行的告警（execution §4.3.1 的
// "只记 warning，不重试、不影响状态"口径）：
//
//   - `secret_env_unbound`：`tasks.secret_env` 里只声明了变量名、没有映射到
//     Secret 名与 key（`03-data-view.md §4.6` 的 `secret_ref` 为空）。执行层**跳过**
//     该变量并告警，不 fail——"required 且完全没绑定"属于提交期校验（C-5.1 第 9 条）。
func RenderWithWarnings(spec ports.JobSpec) (*batchv1.Job, []string, error) {
	if err := validateSpec(spec); err != nil {
		return nil, nil, err
	}
	namespace := spec.Namespace
	if namespace == "" {
		namespace = "testplane"
	}
	subPath, err := OutputSubPath(spec.RunID, spec.TaskID, spec.AttemptNo)
	if err != nil {
		return nil, nil, err
	}
	if spec.OutputDir != "" {
		if err := checkOutputDir(spec.OutputDir, subPath); err != nil {
			return nil, nil, err
		}
	}

	labels, err := buildLabels(spec)
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
	mounts, volumes, err := buildMounts(spec, subPath)
	if err != nil {
		return nil, nil, err
	}

	podLabels := map[string]string{
		LabelApp:     "testplane",
		LabelManaged: "true",
		LabelJobID:   JobID(spec.TaskID, spec.AttemptNo),
	}
	podAnnotations := map[string]string{
		LabelAttempt: spec.AttemptID,
	}
	for k, v := range spec.Annotations {
		podAnnotations[k] = v
	}

	timeout := int64(spec.TimeoutSeconds) + deadlineGraceSeconds
	backoffLimit := int32(0)
	completions := int32(1)
	parallelism := int32(1)
	gracePeriod := int64(30)
	automount := false
	enableServiceLinks := false
	allowPrivEscalation := false
	readOnlyRoot := true
	runAsNonRoot := true
	runAsUser := TestplaneUID
	runAsGroup := TestplaneGID
	fsGroup := TestplaneGID
	seccomp := corev1.SeccompProfileTypeRuntimeDefault

	podSpec := corev1.PodSpec{
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
			Name:            TesterContainerName,
			Image:           spec.Image,
			ImagePullPolicy: imagePullPolicy(spec.Image),
			Command:         []string{entryOr(spec.Entry)},
			Args:            append([]string(nil), spec.Args...),
			Env:             env,
			Resources:       res,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &allowPrivEscalation,
				ReadOnlyRootFilesystem:   &readOnlyRoot,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			VolumeMounts: mounts,
		}},
		Volumes: volumes,
	}
	if spec.ServiceAccount != "" {
		podSpec.ServiceAccountName = spec.ServiceAccount
	}

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
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: podSpec,
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
	case spec.RunID == "":
		return fmt.Errorf("%w: run_id is empty", ErrSpecInvalid)
	case spec.TaskID == "":
		return fmt.Errorf("%w: task_id is empty", ErrSpecInvalid)
	case spec.AttemptID == "":
		return fmt.Errorf("%w: attempt_id is empty", ErrSpecInvalid)
	case spec.AttemptNo < 1:
		return fmt.Errorf("%w: attempt_no must be >= 1, got %d", ErrSpecInvalid, spec.AttemptNo)
	case spec.Image == "":
		return fmt.Errorf("%w: image is empty", ErrSpecInvalid)
	case spec.TimeoutSeconds <= 0 || spec.TimeoutSeconds > maxTimeoutSeconds:
		return fmt.Errorf("%w: timeout_seconds %d out of (0, %d]", ErrSpecInvalid, spec.TimeoutSeconds, maxTimeoutSeconds)
	}
	if spec.TesterRole != "" && spec.TesterRole != "test" && spec.TesterRole != "pre_check" {
		return fmt.Errorf("%w: role %q must be test|pre_check", ErrSpecInvalid, spec.TesterRole)
	}
	if spec.StoragePVC == "" {
		return fmt.Errorf("%w: storage PVC is required for %s", ErrSpecInvalid, OutputMountPath)
	}
	return nil
}

// OutputSubPath 由 ID 直接推导 subPath（§3.1）：<run>/tasks/<task>/attempts/<n>。
// 不解析宿主路径，避免"尽力纠正"出跨 Run 目录。
func OutputSubPath(runID, taskID string, attemptNo int) (string, error) {
	parts := []string{runID, "tasks", taskID, "attempts", strconv.Itoa(attemptNo)}
	for _, p := range parts {
		if !segmentPattern.MatchString(p) || strings.Contains(p, "..") {
			return "", fmt.Errorf("%w: bad segment %q", ErrBadPath, p)
		}
	}
	return path.Join(parts...), nil
}

// checkOutputDir 校验调用方给的宿主路径与推导出的 subPath 一致（§3.1 的"越界即拒绝"）。
func checkOutputDir(outputDir, subPath string) error {
	clean := path.Clean(strings.ReplaceAll(outputDir, "\\", "/"))
	if strings.HasPrefix(clean, "..") || path.IsAbs(clean) && strings.Contains(clean, "..") {
		return fmt.Errorf("%w: %q escapes storage root", ErrBadPath, outputDir)
	}
	if clean != subPath && !strings.HasSuffix(clean, "/"+subPath) {
		return fmt.Errorf("%w: %q does not end with %q", ErrBadPath, outputDir, subPath)
	}
	return nil
}

func entryOr(entry string) string {
	if entry == "" {
		return DefaultEntry
	}
	return entry
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

func buildLabels(spec ports.JobSpec) (map[string]string, error) {
	labels := map[string]string{
		LabelApp:     "testplane",
		LabelManaged: "true",
		LabelJobID:   JobID(spec.TaskID, spec.AttemptNo),
	}
	set := func(k, v string) error {
		if err := checkLabelValue(v); err != nil {
			return fmt.Errorf("%w: label %s: %v", ErrSpecInvalid, k, err)
		}
		labels[k] = v
		return nil
	}
	for key, val := range map[string]string{
		LabelRun:     spec.RunID,
		LabelTask:    spec.TaskID,
		LabelAttempt: strconv.Itoa(spec.AttemptNo),
	} {
		if err := set(key, val); err != nil {
			return nil, err
		}
	}
	if spec.TesterName != "" {
		if err := set(LabelTester, spec.TesterName); err != nil {
			return nil, err
		}
	}
	if spec.TesterRole != "" {
		if err := set(LabelRole, spec.TesterRole); err != nil {
			return nil, err
		}
	}
	for k, v := range spec.Labels {
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
	if spec.TesterVersion != "" {
		ann[AnnTesterVersion] = spec.TesterVersion
	}
	for k, v := range spec.Annotations {
		ann[k] = v
	}
	return ann, nil
}

// buildEnv 渲染容器环境变量（§2.1）：TESTPLANE_* / TZ / HOME 强制覆写，
// 用户变量按 key 字典序，Secret 变量最后追加 valueFrom.secretKeyRef。
//
// 返回的 warnings 是"不影响执行、但要让上游看见"的条目。最有代表性的是
// `secret_env_unbound`：`tasks.secret_env` 只声明了变量名、`secret_ref` 为空
// （03-data-view §4.6 允许这种形状，Tester 侧 `required: false` 就表示可以不给）。
// 执行层只跳过并告警——"声明 required 却没绑定"是提交期校验（C-5.1 第 9 条）的责任，
// 执行器不承担，也不能因此把整个 Attempt 判死。
func buildEnv(spec ports.JobSpec) ([]corev1.EnvVar, []string, error) {
	env := []corev1.EnvVar{
		{Name: TestplaneEnvRunID, Value: spec.RunID},
		{Name: TestplaneEnvTaskID, Value: spec.TaskID},
		{Name: TestplaneEnvAttemptID, Value: spec.AttemptID},
		{Name: TestplaneEnvTZ, Value: DefaultTZ},
		{Name: TestplaneEnvHome, Value: TmpMountPath},
	}
	reserved := map[string]struct{}{
		TestplaneEnvRunID: {}, TestplaneEnvTaskID: {}, TestplaneEnvAttemptID: {},
		TestplaneEnvTZ: {}, TestplaneEnvHome: {},
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := reserved[k]; ok {
			// 执行层覆写：Tester 不得改写平台注入的运行元信息。
			continue
		}
		env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}

	secrets := make([]string, 0, len(spec.SecretEnv))
	secrets = append(secrets, spec.SecretEnv...)
	sort.Strings(secrets)
	var warnings []string
	for _, raw := range secrets {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name, secretName, key, ok := parseSecretEnv(raw)
		if !ok {
			// 未绑定（只有变量名）或形态不认识：跳过 + 告警，绝不 fail Attempt。
			warnings = append(warnings, fmt.Sprintf("secret_env_unbound: %s (skipped)", raw))
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

// parseSecretEnv 解析 "ENV=secretName/key" 或 "ENV=secretName:key"。
// 只给变量名（无映射）返回 ok=false：调用方跳过并告警，不猜 Secret 名与 key。
func parseSecretEnv(raw string) (name, secretName, key string, ok bool) {
	m := secretEnvPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
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

func buildMounts(spec ports.JobSpec, subPath string) ([]corev1.VolumeMount, []corev1.Volume, error) {
	mounts := []corev1.VolumeMount{
		{Name: "output", MountPath: OutputMountPath, SubPath: subPath},
		{Name: "tmp", MountPath: TmpMountPath},
	}
	volumes := []corev1.Volume{
		{
			Name: "output",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: spec.StoragePVC},
			},
		},
		{
			Name: "tmp",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resourcePtr(tmpSizeLimit)},
			},
		},
	}

	if spec.InputDir != "" {
		rel := path.Clean(strings.ReplaceAll(spec.InputDir, "\\", "/"))
		if path.IsAbs(rel) || strings.HasPrefix(rel, "..") || strings.Contains(rel, "..") {
			return nil, nil, fmt.Errorf("%w: input dir %q", ErrBadPath, spec.InputDir)
		}
		mounts = append(mounts, corev1.VolumeMount{
			Name: "input", MountPath: InputMountPath, SubPath: rel, ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "input",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: spec.StoragePVC,
					ReadOnly:  true,
				},
			},
		})
	}

	used := map[string]struct{}{OutputMountPath: {}, TmpMountPath: {}, InputMountPath: {}}
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
		used[m.MountPath] = struct{}{}
		name := m.Name
		if name == "" {
			name = fmt.Sprintf("mnt-%d", i)
		}
		name = sanitizeLabel(name)
		if name == "" {
			name = fmt.Sprintf("mnt-%d", i)
		}
		claim := m.Source
		if claim == "" {
			claim = spec.StoragePVC
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

// JobName 生成 DNS-1123 合法的 Job 名（§2.3）：`tp-<task_id>-a<attempt>`，
// 超长时截断 task 段并追加内容指纹后缀，保证稳定可重放（同一 Attempt 永远同名）。
func JobName(spec ports.JobSpec) string {
	return Naming(spec.RunID, spec.TaskID, spec.AttemptNo)
}

// Naming 是 JobName 的显式形态，便于在别处（GC、日志）复用同一规则。
func Naming(runID, taskID string, attemptNo int) string {
	task := sanitizeLabel(taskID)
	if task == "" {
		task = "task"
	}
	suffix := "-a" + strconv.Itoa(attemptNo)
	name := "tp-" + task + suffix
	if len(name) <= maxJobNameLen {
		return name
	}
	digest := sha256.Sum256([]byte(runID + "/" + taskID + "/" + strconv.Itoa(attemptNo)))
	tail := "-a" + strconv.Itoa(attemptNo) + "-" + hex.EncodeToString(digest[:])[:8]
	keep := maxJobNameLen - len("tp-") - len(tail)
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
	name = "tp-" + trimmed + tail
	if len(name) > maxJobNameLen {
		name = name[:maxJobNameLen]
	}
	return strings.TrimRight(name, "-")
}

// JobID 是与 Job 名同源派生的不透明幂等键（§2.3）：j- + sha256(task/attempt)[:8]。
func JobID(taskID string, attemptNo int) string {
	sum := sha256.Sum256([]byte(taskID + "/" + strconv.Itoa(attemptNo)))
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
