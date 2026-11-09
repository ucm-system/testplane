package domain

import (
	"strings"
	"testing"
)

// D-63：统一挂载配置的校验与保留路径（#74 合并后的唯一常量集）。
func TestNormalizeSharedMounts(t *testing.T) {
	t.Run("合法两条", func(t *testing.T) {
		in := []SharedMountSpec{
			{Name: "models", MountPath: "/models", ReadOnly: true, ClaimName: "c1"},
			{Name: "alt", MountPath: "/mnt/model/", ReadOnly: true, ClaimName: "c1"},
		}
		out, err := NormalizeSharedMounts(in)
		if err != nil {
			t.Fatalf("NormalizeSharedMounts: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("len = %d", len(out))
		}
		if out[1].MountPath != "/mnt/model" {
			t.Errorf("尾部斜杠未归一: %q", out[1].MountPath)
		}
	})
	t.Run("claimName 必填", func(t *testing.T) {
		if _, err := NormalizeSharedMounts([]SharedMountSpec{{MountPath: "/models"}}); err == nil ||
			!strings.Contains(err.Error(), "claimName") {
			t.Fatalf("err = %v, want claimName 必填", err)
		}
	})
	t.Run("绝对路径", func(t *testing.T) {
		if _, err := NormalizeSharedMounts([]SharedMountSpec{{MountPath: "models", ClaimName: "c"}}); err == nil {
			t.Fatal("相对路径应被拒绝")
		}
	})
	t.Run("不得占用平台保留路径", func(t *testing.T) {
		for _, p := range []string{WorkspaceOutputPath, WorkspaceInputPath, TmpPath, TesterDirPath} {
			if _, err := NormalizeSharedMounts([]SharedMountSpec{{MountPath: p, ClaimName: "c"}}); err == nil {
				t.Errorf("%s 应被拒绝（平台保留路径）", p)
			}
		}
	})
	t.Run("name/路径唯一", func(t *testing.T) {
		if _, err := NormalizeSharedMounts([]SharedMountSpec{
			{Name: "a", MountPath: "/models", ClaimName: "c"},
			{Name: "a", MountPath: "/mnt/model", ClaimName: "c"},
		}); err == nil {
			t.Error("重名应被拒绝")
		}
		if _, err := NormalizeSharedMounts([]SharedMountSpec{
			{Name: "a", MountPath: "/models", ClaimName: "c"},
			{Name: "b", MountPath: "/models", ClaimName: "c"},
		}); err == nil {
			t.Error("重路径应被拒绝")
		}
	})
}

// #74：保留路径只有一套（registry / render / 提交期共用 domain 的常量）。
func TestReservedMountPathsSingleSource(t *testing.T) {
	shared := []SharedMountSpec{{Name: "models", MountPath: "/models", ClaimName: "c"}}
	reserved := ReservedMountPaths(shared)
	for _, p := range []string{WorkspaceOutputPath, WorkspaceInputPath, TmpPath, TesterDirPath, "/models"} {
		if _, ok := reserved[p]; !ok {
			t.Errorf("%s 不在保留集合里（#74 的两套集合回归）", p)
		}
	}
	if len(reserved) != 5 {
		t.Errorf("reserved = %v, want 4 平台 + 1 统一挂载", reserved)
	}
}

// 提交期冲突：Tester mounts 与统一挂载/保留路径同路径 → 冲突（不做只读交集）。
func TestMountPathConflicts(t *testing.T) {
	shared := []SharedMountSpec{{Name: "models", MountPath: "/models", ClaimName: "c"}}
	cases := []struct {
		path string
		want bool
	}{
		{"/models", true},
		{"/workspace/output", true},
		{"/tester", true},
		{"/data", false},
	}
	for _, c := range cases {
		got := MountPathConflicts([]MountSpec{{Name: "m", MountPath: c.path}}, shared)
		if (len(got) > 0) != c.want {
			t.Errorf("path %q: conflicts=%v, want %v", c.path, got, c.want)
		}
	}
}
