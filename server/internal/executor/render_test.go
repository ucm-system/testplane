package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// sampleSpec 复刻 design/impl/execution.md §2.2 的渲染输入（Job Spec 2.0：
// 三元组定位、command = runner entry、args 恒空、env 全 AG_*）。
func sampleSpec() ports.JobSpec {
	return ports.JobSpec{
		SpecVersion:   ports.JobSpecVersion2,
		TaskID:        "t-0001",
		AttemptNo:     1,
		Seq:           2,
		RunnerName:    "endpoint-probe",
		RunnerVersion: "1.0.0",
		Role:          domain.RolePreCheck,
		Image:         "registry.dev.huawei.com/argus/endpoint-probe:1.0.0",
		Command:       []string{"/runner/run.sh"}, // runner.yaml.entry（exec form 数组，D-66）
		Env: map[string]string{
			"AG_SERVED_URL":        "http://10.0.0.10:8000/v1",
			"AG_SERVED_MODEL_NAME": "Qwen3-32B",
			"ENDPOINT_EXTRA":       "x",
			"TZ":                   "UTC",
		},
		Namespace: "argus",
		Resources: domain.ResourceRequest{
			CPU: "1", Memory: "256Mi",
			GPU: domain.GPURequest{Resource: "nvidia.com/gpu", Count: 0},
		},
		OutputDir:      "/storage-root/t-0001/attempts/1/runners/2",
		InputDir:       "/storage-root/t-0001/attempts/1/input/2",
		StoragePVC:     "argus-storage",
		TimeoutSeconds: 900,
		SecretEnv: []ports.SecretEnvRef{
			{Name: "ENDPOINT_API_KEY", SecretRef: "argus-secret-endpoint-prod-qwen/api_key"},
		},
		Annotations: map[string]string{},
	}
}

func TestRenderMatchesDesignSection22(t *testing.T) {
	job, err := Render(sampleSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if job.Name != "argus-t-0001-1-2" {
		t.Errorf("job name = %q, want argus-t-0001-1-2", job.Name)
	}
	if job.Namespace != "argus" {
		t.Errorf("namespace = %q", job.Namespace)
	}

	// 固定渲染项（§2.1 末段）。
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.Completions == nil || *job.Spec.Completions != 1 {
		t.Errorf("completions = %v, want 1", job.Spec.Completions)
	}
	if job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 {
		t.Errorf("parallelism = %v, want 1", job.Spec.Parallelism)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 900+60 {
		t.Errorf("activeDeadlineSeconds = %v, want 960", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("ttlSecondsAfterFinished must not be set")
	}
	if job.Spec.Selector != nil {
		t.Errorf("selector must not be set")
	}

	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken must be false（D-72：runner 不绑 ServiceAccount）")
	}
	if pod.ServiceAccountName != "" {
		t.Errorf("serviceAccountName = %q, want empty", pod.ServiceAccountName)
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Errorf("enableServiceLinks must be false")
	}
	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != 30 {
		t.Errorf("terminationGracePeriodSeconds = %v", pod.TerminationGracePeriodSeconds)
	}
	sc := pod.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("pod securityContext.runAsNonRoot must be true: %+v", sc)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 10001 {
		t.Errorf("runAsUser = %v", sc.RunAsUser)
	}
	if sc.FSGroup == nil || *sc.FSGroup != 10001 {
		t.Errorf("fsGroup = %v", sc.FSGroup)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccompProfile = %+v", sc.SeccompProfile)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Name != RunnerContainerName {
		t.Errorf("container name = %q, want %q", c.Name, RunnerContainerName)
	}
	if c.Image != sampleSpec().Image {
		t.Errorf("image = %q (platform must not rewrite the reference, K-3)", c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("imagePullPolicy = %q, want IfNotPresent for a pinned tag", c.ImagePullPolicy)
	}
	// command = runner.yaml.entry（exec form 数组）；args 恒空（D-66）。
	if len(c.Command) != 1 || c.Command[0] != "/runner/run.sh" {
		t.Errorf("command = %#v, want [/runner/run.sh]", c.Command)
	}
	if len(c.Args) != 0 {
		t.Errorf("args = %#v, want empty（D-66：业务输入走输入文件，argv 恒空）", c.Args)
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("container must run with readOnlyRootFilesystem")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %+v", c.SecurityContext.Capabilities)
	}

	// 环境变量：AG_* 元信息强制注入，公共参数与业务变量按字典序，TZ/HOME 覆写。
	env := map[string]corev1.EnvVar{}
	var names []string
	for _, e := range c.Env {
		env[e.Name] = e
		names = append(names, e.Name)
	}
	for _, want := range []string{
		EnvTaskID, EnvAttempt, EnvRunnerSeq, EnvParamsFile, EnvOutputDir,
		EnvTZ, EnvHome, "AG_SERVED_URL", "AG_SERVED_MODEL_NAME", "ENDPOINT_EXTRA", "ENDPOINT_API_KEY",
	} {
		if _, ok := env[want]; !ok {
			t.Errorf("env %s missing (have %v)", want, names)
		}
	}
	if got := env[EnvTaskID].Value; got != "t-0001" {
		t.Errorf("AG_TASK_ID = %q", got)
	}
	if got := env[EnvAttempt].Value; got != "1" {
		t.Errorf("AG_ATTEMPT = %q", got)
	}
	if got := env[EnvRunnerSeq].Value; got != "2" {
		t.Errorf("AG_RUNNER_SEQ = %q", got)
	}
	if got := env[EnvParamsFile].Value; got != "/workspace/input/params.jsonl" {
		t.Errorf("AG_PARAMS_FILE = %q", got)
	}
	if got := env[EnvOutputDir].Value; got != OutputMountPath {
		t.Errorf("AG_OUTPUT_DIR = %q", got)
	}
	if got := env[EnvTZ].Value; got != DefaultTZ {
		t.Errorf("TZ must be overridden to %q, got %q", DefaultTZ, got)
	}
	if got := env[EnvHome].Value; got != "/tmp" {
		t.Errorf("HOME = %q", got)
	}
	sec := env["ENDPOINT_API_KEY"]
	if sec.ValueFrom == nil || sec.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("secret env must use secretKeyRef: %+v", sec)
	}
	if sec.ValueFrom.SecretKeyRef.Name != "argus-secret-endpoint-prod-qwen" ||
		sec.ValueFrom.SecretKeyRef.Key != "api_key" {
		t.Errorf("secretKeyRef = %+v", sec.ValueFrom.SecretKeyRef)
	}

	// 资源：Guaranteed（requests == limits），GPU count 0 时不渲染。
	if q := c.Resources.Requests[corev1.ResourceCPU]; q.String() != "1" {
		t.Errorf("cpu request = %s", q.String())
	}
	if q := c.Resources.Limits[corev1.ResourceMemory]; q.String() != "256Mi" {
		t.Errorf("memory limit = %s", q.String())
	}
	if _, ok := c.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Errorf("gpu must not be rendered when count == 0")
	}

	// 挂载：input 只读 + output 读写，都带 subPath（attempts/<n>/...）；/tmp 是 emptyDir。
	// 同 claim 复用一条卷目（output 名），因此**按 MountPath** 断言而非按卷名。
	m := map[string]*corev1.VolumeMount{}
	for i := range c.VolumeMounts {
		m[c.VolumeMounts[i].MountPath] = &c.VolumeMounts[i]
	}
	if v := m[OutputMountPath]; v == nil ||
		v.SubPath != "t-0001/attempts/1/runners/2" || v.ReadOnly {
		t.Fatalf("output mount = %+v", v)
	}
	if v := m[InputMountPath]; v == nil ||
		v.SubPath != "t-0001/attempts/1/input/2" || !v.ReadOnly || v.Name != "output" {
		t.Fatalf("input mount = %+v（应复用 output 卷名）", v)
	}
	if v := m["/tmp"]; v == nil || v.MountPath != "/tmp" {
		t.Fatalf("tmp mount = %+v", v)
	}
	vols := map[string]*corev1.Volume{}
	for i := range pod.Volumes {
		vols[pod.Volumes[i].Name] = &pod.Volumes[i]
	}
	// 同 claim 只有一条 volume 声明（output 复用给 input 挂载）：两条同 claim 卷目
	// 会让 kubelet 对同一 hostPath 目录二次 bind，在 Docker Desktop drvfs 上悬挂（实测）。
	if v := vols["output"]; v == nil || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "argus-storage" {
		t.Errorf("output volume = %+v (PVC 统一 argus-storage，D-41)", v)
	}
	if v, ok := vols["input"]; ok {
		t.Errorf("input 不应再有独立 volume 声明（同 claim 复用 output 卷目）: %+v", v)
	}
	if n := len(pod.Volumes); n != 2 { // output(PVC) + tmp(emptyDir)
		t.Errorf("volumes 数量 = %d，期望 2（同 claim 去重后）", n)
	}
	// StorageRO=true 时 claim 级置只读（Profile 的只读存储开关；input 的只读由 mount 级强制）。
	specRO := sampleSpec()
	specRO.StorageRO = true
	jobRO, err := Render(specRO)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range jobRO.Spec.Template.Spec.Volumes {
		if v.Name == "output" && (v.PersistentVolumeClaim == nil || !v.PersistentVolumeClaim.ReadOnly) {
			t.Errorf("StorageRO=true must set claim-level read-only on the storage volume: %+v", v)
		}
	}

	// label / annotation（§2.3，D-66：argus.io/*）。
	for k, want := range map[string]string{
		LabelManaged:   "true",
		LabelTask:      "t-0001",
		LabelAttempt:   "1",
		LabelRunnerSeq: "2",
		LabelRole:      "pre_check",
		LabelApp:       "argus",
	} {
		if got := job.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if got := job.Labels[LabelJobID]; got != JobID("t-0001", 1, 2) {
		t.Errorf("job-id label = %q, want %q", got, JobID("t-0001", 1, 2))
	}
	for _, k := range []string{AnnSpecHash, AnnRunnerVersion, AnnTimeoutSeconds, AnnRequestedBy} {
		if job.Annotations[k] == "" {
			t.Errorf("annotation %s missing", k)
		}
	}
	if !strings.HasPrefix(job.Annotations[AnnSpecHash], "sha256:") {
		t.Errorf("spec-hash = %q", job.Annotations[AnnSpecHash])
	}
	if got := job.Spec.Template.Annotations[AnnRunnerKey]; got != "t-0001:1:2" {
		t.Errorf("pod template runner-key = %q, want t-0001:1:2", got)
	}
}

// D-66：AG_PARAMS_FILE 的扩展名由 input_format 决定——编排层经 spec.Env 覆盖，
// 执行层透传（不感知 input_format，D-67）。
func TestRenderParamsFileFromEnv(t *testing.T) {
	spec := sampleSpec()
	spec.Env = map[string]string{"AG_PARAMS_FILE": "/workspace/input/params.yaml"}
	job, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == EnvParamsFile {
			if e.Value != "/workspace/input/params.yaml" {
				t.Errorf("AG_PARAMS_FILE = %q, want the orchestrator-provided path", e.Value)
			}
			return
		}
	}
	t.Fatal("AG_PARAMS_FILE missing")
}

// D-72 硬规则：渲染出的 Job 绝不能带 ownerReferences（Job 回收的级联删除会
// 杀掉 Genet 管理的活服务）。
func TestRenderNeverSetsOwnerReferences(t *testing.T) {
	job, err := Render(sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(job.OwnerReferences) != 0 {
		t.Errorf("job ownerReferences = %+v, want none (D-72)", job.OwnerReferences)
	}
	if len(job.Spec.Template.OwnerReferences) != 0 {
		t.Errorf("pod template ownerReferences = %+v, want none (D-72)", job.Spec.Template.OwnerReferences)
	}
}

func TestSpecHashIsStableAndSensitive(t *testing.T) {
	a, err := Render(sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	if a.Annotations[AnnSpecHash] != b.Annotations[AnnSpecHash] {
		t.Errorf("spec-hash must be stable across renders")
	}

	spec := sampleSpec()
	spec.Command = []string{"/runner/run.sh", "--wait_timeout_seconds=600"}
	c, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if a.Annotations[AnnSpecHash] == c.Annotations[AnnSpecHash] {
		t.Errorf("spec-hash must change when command changes")
	}
}

func TestJobNameIsDNS1123AndDeterministic(t *testing.T) {
	want := Naming("t-0007", 3, 1)
	if want != "argus-t-0007-3-1" {
		t.Errorf("Naming = %q", want)
	}
	if again := Naming("t-0007", 3, 1); again != want {
		t.Errorf("Naming is not deterministic: %q vs %q", again, want)
	}

	long := Naming(strings.Repeat("t-very-long-task-identifier", 3), 1, 12)
	if len(long) > maxJobNameLen {
		t.Errorf("name %q exceeds %d chars", long, maxJobNameLen)
	}
	if strings.HasSuffix(long, "-") || strings.Contains(long, "_") || strings.ToLower(long) != long {
		t.Errorf("name %q is not a valid DNS-1123 label", long)
	}
}

func TestJobIDDeterministic(t *testing.T) {
	a := JobID("t-0001", 1, 2)
	if a != JobID("t-0001", 1, 2) {
		t.Errorf("JobID not deterministic")
	}
	if a == JobID("t-0001", 1, 3) || a == JobID("t-0001", 2, 2) {
		t.Errorf("JobID must differ across attempt/seq")
	}
	if !strings.HasPrefix(a, "j-") || len(a) != len("j-")+8 {
		t.Errorf("JobID = %q, want j-XXXXXXXX", a)
	}
}

func TestRenderRejectsInvalidSpecs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ports.JobSpec)
		target error
	}{
		{"wrong spec version", func(s *ports.JobSpec) { s.SpecVersion = "1.0" }, ErrBadSpecVersion},
		{"empty task", func(s *ports.JobSpec) { s.TaskID = "" }, ErrSpecInvalid},
		{"empty image", func(s *ports.JobSpec) { s.Image = "" }, ErrSpecInvalid},
		{"zero attempt", func(s *ports.JobSpec) { s.AttemptNo = 0 }, ErrSpecInvalid},
		{"zero seq", func(s *ports.JobSpec) { s.Seq = 0 }, ErrSpecInvalid},
		{"empty command", func(s *ports.JobSpec) { s.Command = nil }, ErrSpecInvalid},
		{"bad role", func(s *ports.JobSpec) { s.Role = "bench" }, ErrSpecInvalid},
		{"timeout over max", func(s *ports.JobSpec) { s.TimeoutSeconds = maxTimeoutSeconds + 1 }, ErrSpecInvalid},
		{"empty output dir", func(s *ports.JobSpec) { s.OutputDir = "" }, ErrSpecInvalid},
		{"output dir mismatch", func(s *ports.JobSpec) { s.OutputDir = "/storage-root/other/attempts/1/runners/2" }, ErrBadPath},
		{"input dir mismatch", func(s *ports.JobSpec) { s.InputDir = "/storage-root/t-0001/attempts/1/input/9" }, ErrBadPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := sampleSpec()
			tc.mutate(&spec)
			_, err := Render(spec)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !errors.Is(err, tc.target) {
				t.Fatalf("error = %v, want %v", err, tc.target)
			}
		})
	}
}

// D-41：StoragePVC 缺省时渲染统一 PVC 名 argus-storage。
func TestRenderDefaultsStoragePVC(t *testing.T) {
	spec := sampleSpec()
	spec.StoragePVC = ""
	job, err := Render(spec)
	if err != nil {
		t.Fatalf("empty StoragePVC must default to %q: %v", DefaultStoragePVC, err)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != DefaultStoragePVC {
			t.Errorf("pvc claim = %q, want %q", v.PersistentVolumeClaim.ClaimName, DefaultStoragePVC)
		}
	}
}

func TestRenderGPUAndQuantity(t *testing.T) {
	spec := sampleSpec()
	spec.Resources = domain.ResourceRequest{
		CPU: "4", Memory: "8Gi",
		GPU: domain.GPURequest{Resource: "huawei.com/Ascend910", Count: 2},
	}
	job, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	res := job.Spec.Template.Spec.Containers[0].Resources
	gpu := corev1.ResourceName("huawei.com/Ascend910")
	if q, ok := res.Requests[gpu]; !ok || q.Value() != 2 {
		t.Errorf("gpu request = %v (%v)", res.Requests, ok)
	}
	if q, ok := res.Limits[gpu]; !ok || q.Value() != 2 {
		t.Errorf("gpu limits must equal requests")
	}
	if q := res.Limits[corev1.ResourceMemory]; q.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("memory limit = %s", q.String())
	}
}

func TestRenderImagePullPolicy(t *testing.T) {
	cases := map[string]corev1.PullPolicy{
		"repo/app:1.0.0": corev1.PullIfNotPresent,
		"repo/app@sha256:" + strings.Repeat("a", 64): corev1.PullIfNotPresent,
		"repo/app:latest":            corev1.PullAlways,
		"repo/app":                   corev1.PullAlways,
		"registry:5000/repo/app:2.0": corev1.PullIfNotPresent,
	}
	for image, want := range cases {
		if got := imagePullPolicy(image); got != want {
			t.Errorf("imagePullPolicy(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestSubPathValidation(t *testing.T) {
	goodOut, err := OutputSubPath("t-1", 2, 3)
	if err != nil || goodOut != "t-1/attempts/2/runners/3" {
		t.Fatalf("OutputSubPath = %q, %v", goodOut, err)
	}
	goodIn, err := InputSubPath("t-1", 2, 3)
	if err != nil || goodIn != "t-1/attempts/2/input/3" {
		t.Fatalf("InputSubPath = %q, %v", goodIn, err)
	}
	for _, bad := range []string{"../escape", "r 1"} {
		if _, err := OutputSubPath(bad, 1, 1); err == nil {
			t.Errorf("OutputSubPath(%q) must fail", bad)
		}
		if _, err := InputSubPath(bad, 1, 1); err == nil {
			t.Errorf("InputSubPath(%q) must fail", bad)
		}
	}
}

func TestParseImageDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	cases := map[string]string{
		"docker-pullable://registry/repo@sha256:" + strings.Repeat("ab", 32): digest,
		"docker://registry/repo@" + digest:                                   digest,
		"registry/repo:1.0.0":                                                "",
		"":                                                                   "",
		"docker-pullable://repo@sha256:tooShort":                             "",
	}
	for in, want := range cases {
		if got := ParseImageDigest(in); got != want {
			t.Errorf("ParseImageDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderSkipsUnboundSecretEnv 覆盖防御性问题：
// secret_env 只声明变量名（secret_ref 为空）时，执行层必须**跳过 + 告警**，
// 不能 fail 执行记录——"required 却没绑定"是提交期校验的责任（C-5.1）。
func TestRenderSkipsUnboundSecretEnv(t *testing.T) {
	spec := sampleSpec()
	spec.SecretEnv = []ports.SecretEnvRef{
		{Name: "ENDPOINT_API_KEY"},                                      // 未绑定：跳过 + WARN
		{Name: "BOUND_KEY", SecretRef: "argus-secret-endpoint/api_key"}, // 绑定：生成 secretKeyRef
		{Name: "COLON_KEY", SecretRef: "argus-secret-endpoint:token"},   // 冒号形态：同样接受
	}
	job, warnings, err := RenderWithWarnings(spec)
	if err != nil {
		t.Fatalf("unbound secret env must not fail rendering: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "secret_env_unbound") ||
		!strings.Contains(warnings[0], "ENDPOINT_API_KEY") {
		t.Fatalf("warnings = %#v, want one secret_env_unbound for ENDPOINT_API_KEY", warnings)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	if _, ok := env["ENDPOINT_API_KEY"]; ok {
		t.Errorf("unbound secret env must be skipped, got %+v", env["ENDPOINT_API_KEY"])
	}
	for name, wantKey := range map[string]string{"BOUND_KEY": "api_key", "COLON_KEY": "token"} {
		v, ok := env[name]
		if !ok || v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("env %s must use secretKeyRef: %+v", name, v)
		}
		if v.ValueFrom.SecretKeyRef.Name != "argus-secret-endpoint" || v.ValueFrom.SecretKeyRef.Key != wantKey {
			t.Errorf("env %s secretKeyRef = %+v", name, v.ValueFrom.SecretKeyRef)
		}
	}

	// Render 兼容入口不返回告警，但结果一致。
	plain, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if plain.Annotations[AnnSpecHash] != job.Annotations[AnnSpecHash] {
		t.Errorf("Render and RenderWithWarnings must produce the same job")
	}
}

// TestRenderEmptySecretEnvIsFine 保证空占位项（name 与 ref 都空）不会触发告警。
func TestRenderEmptySecretEnvIsFine(t *testing.T) {
	spec := sampleSpec()
	spec.SecretEnv = []ports.SecretEnvRef{{}, {Name: "   ", SecretRef: "   "}}
	job, warnings, err := RenderWithWarnings(spec)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if job == nil {
		t.Fatal("job is nil")
	}
}

// TestSubmitWarnsOnUnboundSecret 确认告警经执行器日志钩子落出（不影响 Submit）。
func TestSubmitWarnsOnUnboundSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var logged []string
	ex := NewWithNamespace(cs, "argus", Options{
		PollInterval: 20 * time.Millisecond,
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	})
	spec := sampleSpec()
	spec.SecretEnv = []ports.SecretEnvRef{{Name: "ENDPOINT_API_KEY"}}
	if _, err := ex.Submit(context.Background(), spec); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "secret_env_unbound") {
		t.Fatalf("logged = %#v, want a secret_env_unbound warning", logged)
	}
}
