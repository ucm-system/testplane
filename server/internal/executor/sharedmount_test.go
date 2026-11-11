package executor

import (
	"strings"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// baseSpec 是最小可渲染 JobSpec（mounts/pvc 由具体用例覆盖）。
func baseSpec() ports.JobSpec {
	return ports.JobSpec{
		SpecVersion: ports.JobSpecVersion2,
		TaskID:      "t-1", AttemptNo: 1, Seq: 1,
		RunnerName: "probe", RunnerVersion: "1.0.0",
		Role:      domain.RoleTest,
		Image:     "i:1",
		Command:   []string{"/runner/run.sh"},
		Namespace: "argus", StoragePVC: "artifacts",
		OutputDir:      "storage/t-1/attempts/1/runners/1",
		TimeoutSeconds: 600,
	}
}

// D-63：统一挂载注入（PVC source，不用内联 nfs —— restricted PSA 会拒绝）。
func TestRenderInjectsSharedMounts(t *testing.T) {
	spec := baseSpec()
	spec.SharedMounts = []domain.SharedMountSpec{
		{Name: "models", MountPath: "/models", ReadOnly: true, ClaimName: "models-pvc"},
	}
	job, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	pod := job.Spec.Template.Spec
	found := false
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "models-pvc" {
			if v.NFS != nil {
				t.Fatal("不许用内联 nfs（restricted PSA 拒绝）")
			}
			if !v.PersistentVolumeClaim.ReadOnly {
				t.Error("volume 应只读")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("没有注入 claimName=models-pvc 的 volume: %+v", pod.Volumes)
	}
	mounted := false
	for _, c := range pod.Containers[0].VolumeMounts {
		if c.MountPath == "/models" && c.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("没有挂载 /models: %+v", pod.Containers[0].VolumeMounts)
	}
}

// Profile 关闭（sharedMounts: false）→ 不注入。
func TestRenderSharedMountsDisabledByProfile(t *testing.T) {
	spec := baseSpec()
	spec.SharedMounts = []domain.SharedMountSpec{{Name: "models", MountPath: "/models", ClaimName: "models-pvc"}}
	spec.SharedMountsDisabled = true
	job, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "models-pvc" {
			t.Fatal("Profile 关闭时不该注入统一挂载")
		}
	}
}

// 冲突：Runner mounts 与统一挂载同路径 → 渲染期也必须拒绝（双保险）。
func TestRenderRejectsSharedMountPathConflict(t *testing.T) {
	spec := baseSpec()
	spec.SharedMounts = []domain.SharedMountSpec{{Name: "models", MountPath: "/models", ClaimName: "m"}}
	spec.Mounts = []domain.MountSpec{{Name: "mine", MountPath: "/models"}}
	if _, err := Render(spec); err == nil {
		t.Fatal("同路径应被拒绝")
	}
}

// volume 名去重：两个 Runner mount 的 name 经 sanitize 后相同 → 必须生成唯一卷名，
// 否则 K8s 以 "Duplicate value" 拒绝整份 Pod（研究 #69 点出的既有隐患）。
func TestRenderDedupesVolumeNames(t *testing.T) {
	spec := baseSpec()
	spec.Mounts = []domain.MountSpec{
		{Name: "data", MountPath: "/data-a"},
		{Name: "data", MountPath: "/data-b"}, // sanitize 后同名
		{Name: "Data", MountPath: "/data-c"}, // sanitize 后仍同名
	}
	job, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	seen := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if prev, dup := seen[v.Name]; dup {
			t.Fatalf("volume 名重复 %q（%s / %s）", v.Name, prev, v.Name)
		}
		seen[v.Name] = v.Name
	}
	// 统一挂载与 Runner mount 也用同一命名空间：再加一条同名的一起渲染。
	spec.SharedMounts = []domain.SharedMountSpec{{Name: "data", MountPath: "/models", ClaimName: "m"}}
	job, err = Render(spec)
	if err != nil {
		t.Fatalf("Render(shared): %v", err)
	}
	names := map[string]bool{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if names[v.Name] {
			t.Fatalf("volume 名重复（含统一挂载）: %q", v.Name)
		}
		names[v.Name] = true
	}
}

// 保留路径只剩一套（#74）：/runner 在渲染期也必须被拒（以前只在提交期拒）。
func TestRenderRejectsRunnerDirMount(t *testing.T) {
	spec := baseSpec()
	spec.Mounts = []domain.MountSpec{{Name: "t", MountPath: domain.RunnerDirPath}}
	_, err := Render(spec)
	if err == nil || !strings.Contains(err.Error(), domain.RunnerDirPath) {
		t.Fatalf("err = %v, want %s 冲突", err, domain.RunnerDirPath)
	}
}
