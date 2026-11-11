package main

import (
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/registry"
)

// wire 适配层的编译期断言与小 round-trip：
//   - registryStorage 满足 registry.Store（编译期）；Record ↔ *store.Runner 投影无损；
//   - *executor.Executor 满足 ports.Executor（S2 已落地，编译期断言）。

var _ registry.Store = (*registryStorage)(nil)
var _ ports.Executor = (*executor.Executor)(nil)

func mustEnvDecl() map[string]domain.EnvVar {
	return map[string]domain.EnvVar{
		"AG_TOKEN": {Required: true, Secret: true},
	}
}

func TestRecordRunnerRoundTrip(t *testing.T) {
	rec := registry.Record{
		ContentSHA: "abc123",
		SourceFile: "runners/aisbench-acc/runner.yaml",
	}
	rec.Spec.Name = "aisbench-acc"
	rec.Spec.Version = "1.0.0"
	rec.Spec.Role = "test"
	rec.Spec.OnFailure = "continue"
	rec.Spec.Entry = []string{"/runner/run.sh"}
	rec.Spec.Exports = []string{"ACC_SCORE"}
	rec.Spec.Resources.CPU = "1"
	rec.Spec.Resources.Memory = "256Mi"
	rec.Spec.Env = mustEnvDecl()

	n := recordToRunner(rec)
	back := runnerToRecord(n)
	if back.ContentSHA != rec.ContentSHA || back.SourceFile != rec.SourceFile {
		t.Fatalf("元数据丢失: %+v", back)
	}
	if back.Spec.Name != rec.Spec.Name || back.Spec.Version != rec.Spec.Version ||
		back.Spec.Role != rec.Spec.Role || back.Spec.OnFailure != rec.Spec.OnFailure {
		t.Fatalf("Spec 标量字段丢失: %+v", back.Spec)
	}
	if len(back.Spec.Entry) != len(rec.Spec.Entry) || len(back.Spec.Exports) != len(rec.Spec.Exports) {
		t.Fatalf("Spec 数组字段丢失: %+v", back.Spec)
	}
	if back.Spec.Resources.CPU != rec.Spec.Resources.CPU {
		t.Fatalf("Resources 丢失: %+v", back.Spec.Resources)
	}
	if rec.Spec.Env["AG_TOKEN"].Required != back.Spec.Env["AG_TOKEN"].Required {
		t.Fatalf("Env 声明丢失: %+v", back.Spec.Env)
	}
}
