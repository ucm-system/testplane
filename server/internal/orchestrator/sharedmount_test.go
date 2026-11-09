package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestEffectiveSharedMounts_PrefersRuntimeSource 是 D-65 的渲染侧验收：
// 运行时来源（DB）优先于启动种子；来源报错时回落种子；enabled=false 由来源自己过滤为空。
func TestEffectiveSharedMounts_PrefersRuntimeSource(t *testing.T) {
	seed := []domain.SharedMountSpec{{Name: "models", MountPath: "/models", ReadOnly: true, ClaimName: "seed-pvc"}}
	dbv := []domain.SharedMountSpec{{Name: "models", MountPath: "/models", ReadOnly: true, ClaimName: "db-pvc"}}
	task := TaskView{ID: "t-1", Profile: "k8s-npu"}

	// 1) FN 有值 → 用 DB（管理面写入后下一次渲染即生效）。
	c := &Controller{cfg: Config{SharedMounts: seed, SharedMountsFn: func(context.Context) ([]domain.SharedMountSpec, error) {
		return dbv, nil
	}}}
	got := c.effectiveSharedMounts(context.Background(), task)
	if len(got) != 1 || got[0].ClaimName != "db-pvc" {
		t.Fatalf("应优先用运行时来源（db-pvc），实际 %+v", got)
	}

	// 2) FN 报错 → 回落种子（不阻断提交）。
	c = &Controller{cfg: Config{SharedMounts: seed, SharedMountsFn: func(context.Context) ([]domain.SharedMountSpec, error) {
		return nil, errors.New("db down")
	}}}
	got = c.effectiveSharedMounts(context.Background(), task)
	if len(got) != 1 || got[0].ClaimName != "seed-pvc" {
		t.Fatalf("读库失败应回落种子，实际 %+v", got)
	}

	// 3) FN 未接线（nil）→ 种子。
	c = &Controller{cfg: Config{SharedMounts: seed}}
	got = c.effectiveSharedMounts(context.Background(), task)
	if len(got) != 1 || got[0].ClaimName != "seed-pvc" {
		t.Fatalf("未接线时应使用静态配置，实际 %+v", got)
	}

	// 4) 管理面删光 / 全停用 → FN 返回空 → 不注入（渲染层见空列表即不挂）。
	c = &Controller{cfg: Config{SharedMounts: seed, SharedMountsFn: func(context.Context) ([]domain.SharedMountSpec, error) {
		return []domain.SharedMountSpec{}, nil
	}}}
	got = c.effectiveSharedMounts(context.Background(), task)
	if len(got) != 0 {
		t.Fatalf("运行时来源为空时不得回落到种子（否则删了还注入），实际 %+v", got)
	}
}
