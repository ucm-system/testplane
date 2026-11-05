package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// sampleSpec 复刻 design/impl/execution.md §2.2 的渲染输入。
func sampleSpec() ports.JobSpec {
	return ports.JobSpec{
		RunID:         "r-20261103-0001",
		TaskID:        "t-0001",
		AttemptID:     "t-0001-a1",
		AttemptNo:     1,
		TesterName:    "endpoint-probe",
		TesterVersion: "1.0.0",
		TesterRole:    "pre_check",
		Image:         "registry.dev.huawei.com/testplane/endpoint-probe:1.0.0",
		Entry:         "/tester/run.sh",
		Args:          []string{"--wait_timeout_seconds=600", "--retry_interval_seconds=15"},
		Env:           map[string]string{"ENDPOINT_EXTRA": "x", "TZ": "UTC"},
		Namespace:     "testplane",
		Resources: domain.ResourceRequest{
			CPU: "1", Memory: "256Mi",
			GPU: domain.GPURequest{Resource: "nvidia.com/gpu", Count: 0},
		},
		OutputDir:      "/storage-root/r-20261103-0001/tasks/t-0001/attempts/1",
		StoragePVC:     "testplane-artifacts",
		TimeoutSeconds: 900,
		SecretEnv:      []string{"ENDPOINT_API_KEY=tp-secret-endpoint-prod-qwen/api_key"},
		Annotations:    map[string]string{},
	}
}

func TestRenderMatchesDesignSection22(t *testing.T) {
	job, err := Render(sampleSpec())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if job.Name != "tp-t-0001-a1" {
		t.Errorf("job name = %q, want tp-t-0001-a1", job.Name)
	}
	if job.Namespace != "testplane" {
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

	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken must be false")
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
	if c.Name != TesterContainerName {
		t.Errorf("container name = %q", c.Name)
	}
	if c.Image != sampleSpec().Image {
		t.Errorf("image = %q (platform must not rewrite the reference)", c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("imagePullPolicy = %q, want IfNotPresent for a pinned tag", c.ImagePullPolicy)
	}
	if len(c.Command) != 1 || c.Command[0] != "/tester/run.sh" {
		t.Errorf("command = %#v, want [/tester/run.sh]", c.Command)
	}
	if len(c.Args) != 2 || c.Args[1] != "--retry_interval_seconds=15" {
		t.Errorf("args = %#v", c.Args)
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("container must run with readOnlyRootFilesystem")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %+v", c.SecurityContext.Capabilities)
	}

	// 环境变量：平台三元组强制注入，用户变量按字典序，TZ 被覆写。
	env := map[string]corev1.EnvVar{}
	var names []string
	for _, e := range c.Env {
		env[e.Name] = e
		names = append(names, e.Name)
	}
	for _, want := range []string{TestplaneEnvRunID, TestplaneEnvTaskID, TestplaneEnvAttemptID, TestplaneEnvTZ, TestplaneEnvHome, "ENDPOINT_EXTRA", "ENDPOINT_API_KEY"} {
		if _, ok := env[want]; !ok {
			t.Errorf("env %s missing (have %v)", want, names)
		}
	}
	if got := env[TestplaneEnvAttemptID].Value; got != "t-0001-a1" {
		t.Errorf("TESTPLANE_ATTEMPT_ID = %q", got)
	}
	if got := env[TestplaneEnvTZ].Value; got != DefaultTZ {
		t.Errorf("TZ must be overridden to %q, got %q", DefaultTZ, got)
	}
	if got := env[TestplaneEnvHome].Value; got != "/tmp" {
		t.Errorf("HOME = %q", got)
	}
	sec := env["ENDPOINT_API_KEY"]
	if sec.ValueFrom == nil || sec.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("secret env must use secretKeyRef: %+v", sec)
	}
	if sec.ValueFrom.SecretKeyRef.Name != "tp-secret-endpoint-prod-qwen" ||
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

	// 挂载：output 带 subPath，/tmp 是 emptyDir。
	var output, tmp *corev1.VolumeMount
	for i := range c.VolumeMounts {
		switch c.VolumeMounts[i].Name {
		case "output":
			output = &c.VolumeMounts[i]
		case "tmp":
			tmp = &c.VolumeMounts[i]
		}
	}
	if output == nil || output.MountPath != OutputMountPath || output.SubPath != "r-20261103-0001/tasks/t-0001/attempts/1" {
		t.Fatalf("output mount = %+v", output)
	}
	if tmp == nil || tmp.MountPath != "/tmp" {
		t.Fatalf("tmp mount = %+v", tmp)
	}

	// label / annotation。
	for k, want := range map[string]string{
		LabelManaged: "true",
		LabelRun:     "r-20261103-0001",
		LabelTask:    "t-0001",
		LabelAttempt: "1",
		LabelRole:    "pre_check",
		LabelTester:  "endpoint-probe",
		LabelApp:     "testplane",
	} {
		if got := job.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if got := job.Labels[LabelJobID]; got != JobID("t-0001", 1) {
		t.Errorf("job-id label = %q, want %q", got, JobID("t-0001", 1))
	}
	for _, k := range []string{AnnSpecHash, AnnTesterVersion, AnnTimeoutSeconds, AnnRequestedBy} {
		if job.Annotations[k] == "" {
			t.Errorf("annotation %s missing", k)
		}
	}
	if !strings.HasPrefix(job.Annotations[AnnSpecHash], "sha256:") {
		t.Errorf("spec-hash = %q", job.Annotations[AnnSpecHash])
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
	spec.Args = append(spec.Args, "--max_tokens=1")
	c, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if a.Annotations[AnnSpecHash] == c.Annotations[AnnSpecHash] {
		t.Errorf("spec-hash must change when args change")
	}
}

func TestJobNameIsDNS1123AndDeterministic(t *testing.T) {
	spec := sampleSpec()
	spec.TaskID = "t-0007"
	spec.AttemptNo = 3
	want := Naming(spec.RunID, spec.TaskID, spec.AttemptNo)
	if want != "tp-t-0007-a3" {
		t.Errorf("Naming = %q", want)
	}
	if again := Naming(spec.RunID, spec.TaskID, spec.AttemptNo); again != want {
		t.Errorf("Naming is not deterministic: %q vs %q", again, want)
	}

	long := sampleSpec()
	long.TaskID = strings.Repeat("t-very-long-task-identifier", 3)
	name := Naming(long.RunID, long.TaskID, 12)
	if len(name) > maxJobNameLen {
		t.Errorf("name %q exceeds %d chars", name, maxJobNameLen)
	}
	if strings.HasSuffix(name, "-") || strings.Contains(name, "_") || strings.ToLower(name) != name {
		t.Errorf("name %q is not a valid DNS-1123 label", name)
	}
}

func TestRenderRejectsInvalidSpecs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ports.JobSpec)
		target error
	}{
		{"empty task", func(s *ports.JobSpec) { s.TaskID = "" }, ErrSpecInvalid},
		{"empty image", func(s *ports.JobSpec) { s.Image = "" }, ErrSpecInvalid},
		{"zero attempt", func(s *ports.JobSpec) { s.AttemptNo = 0 }, ErrSpecInvalid},
		{"timeout over max", func(s *ports.JobSpec) { s.TimeoutSeconds = maxTimeoutSeconds + 1 }, ErrSpecInvalid},
		{"no storage pvc", func(s *ports.JobSpec) { s.StoragePVC = "" }, ErrSpecInvalid},
		{"output dir mismatch", func(s *ports.JobSpec) { s.OutputDir = "/storage-root/other/tasks/x/attempts/1" }, ErrBadPath},
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
		"repo/app:1.0.0":                    corev1.PullIfNotPresent,
		"repo/app@sha256:" + strings.Repeat("a", 64): corev1.PullIfNotPresent,
		"repo/app:latest":                   corev1.PullAlways,
		"repo/app":                          corev1.PullAlways,
		"registry:5000/repo/app:2.0":        corev1.PullIfNotPresent,
	}
	for image, want := range cases {
		if got := imagePullPolicy(image); got != want {
			t.Errorf("imagePullPolicy(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestOutputSubPathValidation(t *testing.T) {
	good, err := OutputSubPath("r-1", "t-1", 2)
	if err != nil || good != "r-1/tasks/t-1/attempts/2" {
		t.Fatalf("OutputSubPath = %q, %v", good, err)
	}
	for _, bad := range []string{"../escape", "r 1"} {
		if _, err := OutputSubPath(bad, "t-1", 1); err == nil {
			t.Errorf("OutputSubPath(%q) must fail", bad)
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

// TestRenderSkipsUnboundSecretEnv 覆盖 lead 指出的防御性问题：
// `tasks.secret_env` 里只声明变量名（secret_ref 为空）时，执行层必须**跳过 + 告警**，
// 不能 fail Attempt——"required 却没绑定"是提交期校验的责任（C-5.1 第 9 条）。
func TestRenderSkipsUnboundSecretEnv(t *testing.T) {
	spec := sampleSpec()
	spec.SecretEnv = []string{
		"ENDPOINT_API_KEY",                  // 未绑定：跳过 + WARN
		"BOUND_KEY=tp-secret-endpoint/api_key", // 绑定：生成 secretKeyRef
		"COLON_KEY=tp-secret-endpoint:token",   // 冒号形态：同样接受
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
		if v.ValueFrom.SecretKeyRef.Name != "tp-secret-endpoint" || v.ValueFrom.SecretKeyRef.Key != wantKey {
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

// TestRenderEmptySecretEnvIsFine 保证 []string{} / 空串不会触发任何告警。
func TestRenderEmptySecretEnvIsFine(t *testing.T) {
	spec := sampleSpec()
	spec.SecretEnv = []string{"", "   "}
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
	ex := NewWithNamespace(cs, "testplane", Options{
		PollInterval: 20 * time.Millisecond,
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	})
	spec := sampleSpec()
	spec.SecretEnv = []string{"ENDPOINT_API_KEY"}
	if _, err := ex.Submit(context.Background(), spec); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "secret_env_unbound") {
		t.Fatalf("logged = %#v, want a secret_env_unbound warning", logged)
	}
}
