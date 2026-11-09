package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
)

func seedSpec(name, path, claim string) domain.SharedMountSpec {
	return domain.SharedMountSpec{Name: name, MountPath: path, ReadOnly: true, ClaimName: claim}
}

// TestSharedMounts_SeedOnlyOnceAndDBWins 验证 D-65 的数据源优先级：
// 表为空 → env 种子导入一次；之后无论种子怎么变都不再覆盖（管理面才是真源）。
func TestSharedMounts_SeedOnlyOnceAndDBWins(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	// 干净起点（同包测试共享一个库）。
	if _, err := st.Pool().Exec(ctx, `DELETE FROM shared_mounts`); err != nil {
		t.Fatalf("清表: %v", err)
	}

	seeds := []domain.SharedMountSpec{
		seedSpec("models", "/models", "models-pvc"),
		seedSpec("alt-models", "/mnt/model", "models-pvc"),
	}
	n, err := st.SharedMounts.SeedIfEmpty(ctx, seeds)
	if err != nil {
		t.Fatalf("SeedIfEmpty: %v", err)
	}
	if n != 2 {
		t.Fatalf("首次种子应导入 2 条，实际 %d", n)
	}
	rows, err := st.SharedMounts.List(ctx, nil, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("种子后应有 2 行，实际 %d", len(rows))
	}
	for _, r := range rows {
		if !r.Enabled || !r.ReadOnly || r.CreatedBy != "system:seed" {
			t.Fatalf("种子行字段不对: %+v", r)
		}
	}

	// 第二次启动：种子内容变了也不覆盖。
	n, err = st.SharedMounts.SeedIfEmpty(ctx, []domain.SharedMountSpec{
		seedSpec("models", "/models", "other-pvc"),
		seedSpec("brand-new", "/data", "data-pvc"),
	})
	if err != nil {
		t.Fatalf("SeedIfEmpty(2): %v", err)
	}
	if n != 0 {
		t.Fatalf("表非空时不得再导入，实际导入 %d 条", n)
	}
	got, err := st.SharedMounts.Get(ctx, nil, "models")
	if err != nil {
		t.Fatalf("Get models: %v", err)
	}
	if got.ClaimName != "models-pvc" {
		t.Fatalf("DB 是运行时真源：claim 不应被 env 种子覆盖，实际 %s", got.ClaimName)
	}
	if _, err := st.SharedMounts.Get(ctx, nil, "brand-new"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("表非空时新种子不应被导入，err=%v", err)
	}

	// 管理面改一条 → 生效集合跟着变；enabled=false 不参与渲染。
	cur := got
	cur.ClaimName = "models-pvc-v2"
	cur.UpdatedBy = "user:admin"
	if err := st.SharedMounts.Upsert(ctx, nil, cur); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	specs, err := st.SharedMounts.EffectiveSpecs(ctx, nil)
	if err != nil {
		t.Fatalf("EffectiveSpecs: %v", err)
	}
	found := false
	for _, sp := range specs {
		if sp.Name == "models" && sp.ClaimName == "models-pvc-v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("管理面改动应立即反映到生效集合: %+v", specs)
	}

	disabled := cur
	disabled.Enabled = false
	if err := st.SharedMounts.Upsert(ctx, nil, disabled); err != nil {
		t.Fatalf("Upsert(disabled): %v", err)
	}
	specs, _ = st.SharedMounts.EffectiveSpecs(ctx, nil)
	for _, sp := range specs {
		if sp.Name == "models" {
			t.Fatalf("enabled=false 的挂载不应出现在生效集合: %+v", specs)
		}
	}

	// 删除后不再生效。
	if err := st.SharedMounts.Delete(ctx, nil, "models"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	specs, _ = st.SharedMounts.EffectiveSpecs(ctx, nil)
	for _, sp := range specs {
		if sp.Name == "models" {
			t.Fatalf("删除后不应再注入: %+v", specs)
		}
	}
	if err := st.SharedMounts.Delete(ctx, nil, "models"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，err=%v", err)
	}

	// mount_path 唯一（同名 name 之外的冲突由 DB 兜底翻译成 ErrConflict）。
	dup := &SharedMount{Name: "another", MountPath: "/mnt/model", ReadOnly: true, ClaimName: "x", Enabled: true,
		UpdatedBy: "user:admin"}
	if err := st.SharedMounts.Upsert(ctx, nil, dup); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mount_path 重复应 ErrConflict，err=%v", err)
	}
}
