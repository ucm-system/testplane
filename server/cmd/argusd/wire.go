// wire.go 是 cmd/argusd 的装配胶水（v2）：
//
//   - registryStorage：registry.Store（接口）↔ store.RunnerRegistry（实现）的适配；
//   - newExecutor：ports.Executor 的装配点——*executor.Executor 已对齐 ports.JobSpec /
//     ports.JobHandle 冻结接口（S2 波次落地后接线）；
//   - gcTicker：executor.CollectGarbage ↔ orchestrator 维护 tick 的适配（GC 接回），
//     RunnerRunStates 批量查 store、OnStaleRunning 回编排层的超时修复。
//
// 本文件不做任何业务决策：只搬运类型。
package main

import (
	"context"
	"encoding/json"

	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/registry"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 编译期断言由 wire_test.go 承担（ports.Executor / registry.Store）。

// registryStorage 把 store.RunnerRegistry（*store.Runner 行）适配成 registry.Store（Record）。
type registryStorage struct {
	reg *store.RunnerRegistry
}

// runnerToRecord 把库行投影成注册表 Record（jsonb 列原样进 domain.RunnerSpec）。
func runnerToRecord(n *store.Runner) registry.Record {
	rec := registry.Record{
		ContentSHA: n.ContentSHA,
		SourceFile: n.SourceFile,
		Orphaned:   n.Orphaned,
	}
	rec.Spec.Name = n.Name
	rec.Spec.Version = n.Version
	rec.Spec.DisplayName = n.DisplayName
	rec.Spec.Description = n.Description
	rec.Spec.Role = n.Role
	rec.Spec.OnFailure = n.OnFailure
	rec.Spec.InputFormat = n.InputFormat
	rec.Spec.Image = n.Image
	if n.Placeholder != nil {
		rec.Spec.Placeholder = *n.Placeholder
	}
	if n.CleanRunner != nil {
		rec.Spec.CleanRunner = *n.CleanRunner
	}
	_ = unmarshalInto(n.Entry, &rec.Spec.Entry)
	_ = unmarshalInto(n.Resources, &rec.Spec.Resources)
	_ = unmarshalInto(n.Concurrency, &rec.Spec.Concurrency)
	_ = unmarshalInto(n.Mounts, &rec.Spec.Mounts)
	_ = unmarshalInto(n.Parameters, &rec.Spec.Parameters)
	_ = unmarshalInto(n.Exports, &rec.Spec.Exports)
	_ = unmarshalInto(n.Consumes, &rec.Spec.Consumes)
	_ = unmarshalInto(n.EnvDecl, &rec.Spec.Env)
	return rec
}

func recordToRunner(rec registry.Record) *store.Runner {
	n := &store.Runner{
		Name:         rec.Spec.Name,
		Version:      rec.Spec.Version,
		DisplayName:  rec.Spec.DisplayName,
		Description:  rec.Spec.Description,
		Role:         rec.Spec.Role,
		OnFailure:    rec.Spec.OnFailure,
		InputFormat:  rec.Spec.InputFormat,
		Image:        rec.Spec.Image,
		SourceFile:   rec.SourceFile,
		ContentSHA:   rec.ContentSHA,
		Orphaned:     rec.Orphaned,
		RegisteredBy: "registry",
	}
	if rec.Spec.CleanRunner != "" {
		clean := rec.Spec.CleanRunner
		n.CleanRunner = &clean
	}
	if rec.Spec.Placeholder != "" {
		ph := rec.Spec.Placeholder
		n.Placeholder = &ph
	}
	n.Entry = mustMarshal(rec.Spec.Entry)
	n.Resources = mustMarshal(rec.Spec.Resources)
	n.Concurrency = mustMarshal(rec.Spec.Concurrency)
	n.Mounts = mustMarshal(rec.Spec.Mounts)
	n.Parameters = mustMarshal(rec.Spec.Parameters)
	n.Exports = mustMarshal(rec.Spec.Exports)
	n.Consumes = mustMarshal(rec.Spec.Consumes)
	n.EnvDecl = mustMarshal(rec.Spec.Env)
	return n
}

// UpsertRunner 在一个事务内写一行（registry.Store 接口）。
func (s *registryStorage) UpsertRunner(ctx context.Context, rec registry.Record) (bool, error) {
	return s.reg.UpsertRunner(ctx, recordToRunner(rec))
}

// GetRunner 读一行（version 为空取最高版本）。
func (s *registryStorage) GetRunner(ctx context.Context, name, version string) (registry.Record, error) {
	n, err := s.reg.GetRunner(ctx, name, version)
	if err != nil {
		return registry.Record{}, err
	}
	return runnerToRecord(n), nil
}

// ListRunners 列出全部（role 过滤由 Registry 层做）。
func (s *registryStorage) ListRunners(ctx context.Context) ([]registry.Record, error) {
	rows, err := s.reg.ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Record, 0, len(rows))
	for i := range rows {
		out = append(out, runnerToRecord(&rows[i]))
	}
	return out, nil
}

// MarkRunnerOrphaned 把 (name, version) 标记为 orphaned（只标记，不删除）。
func (s *registryStorage) MarkRunnerOrphaned(ctx context.Context, name, version string) error {
	return s.reg.MarkRunnerOrphaned(ctx, name, version)
}

// ---- 小工具 ----------------------------------------------------------------

// gcTicker 把 executor.CollectGarbage 适配成编排层维护 tick（GC 接回点）：
// 编排层只认 GCTick 函数（不 import executor），本类型负责构造 GCOptions——
// RunnerRunStates 批量查 store（禁止 N+1，票 #20-5），OnStaleRunning 回编排层的
// MarkStaleRunnerTimeout（stale 由调用方闭包注入，规避 Scheduler 构造先后依赖）。
type gcTicker struct {
	st    *store.Store
	exec  *executor.Executor
	stale func(ctx context.Context, taskID string, attemptNo, seq int) error
}

// Tick 跑一轮 GC（execution §6.3）。返回错误只表示本轮 List/查库失败，调用方记日志。
func (g *gcTicker) Tick(ctx context.Context) error {
	_, err := g.exec.CollectGarbage(ctx, executor.GCOptions{
		RunnerRunStates: func(ctx context.Context, jobNames []string) (map[string]executor.RunnerRunState, error) {
			states, err := g.st.RunnerRuns.StatesByJobNames(ctx, nil, jobNames)
			if err != nil {
				return nil, err
			}
			out := make(map[string]executor.RunnerRunState, len(states))
			for job, s := range states {
				out[job] = executor.RunnerRunState{Found: s.Found, Terminal: s.Terminal}
			}
			return out, nil
		},
		OnStaleRunning: func(ctx context.Context, job executor.ManagedJob) error {
			if g.stale == nil || job.TaskID == "" {
				return nil
			}
			return g.stale(ctx, job.TaskID, job.AttemptNo, job.Seq)
		},
	})
	return err
}

func unmarshalInto(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
