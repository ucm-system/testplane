package store

import (
	"context"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// #45-5：SQL 里的终态集合必须等于 domain.TerminalRunStatusStrings()。
//
// 以前 run_repo.go:266 手抄 5 个终态字面量，改 domain 的集合就漂。这条用例直接改库里的
// status 到每个终态，断言 `ListActiveEndpoints` 一律不返回（改回手抄版本、漏一个就红）。
func TestListActiveEndpoints_ExcludesAllTerminalStatuses(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	ep := `{"key":"http://ep:8000","url":"http://ep:8000/v1","model_name":"m"}`
	ids := make([]string, 0, len(domain.TerminalRunStatusStrings()))
	for _, status := range domain.TerminalRunStatusStrings() {
		runID := NewRunID(time.Now())
		if _, _, err := st.CreateRun(ctx, SubmitParams{
			Run: &Run{
				ID: runID, Name: "terminal-" + status, Kind: "run",
				Manifest: []byte(`{}`), Resolved: []byte(`{}`),
				CleanupPolicy: "always", SubmittedBy: "dev", Endpoint: []byte(ep),
			},
			Actor: "user:dev",
		}); err != nil {
			t.Fatalf("CreateRun(%s): %v", status, err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE runs SET status = $2 WHERE id = $1`, runID, status); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
		ids = append(ids, runID)
	}

	rows, err := st.Runs.ListActiveEndpoints(ctx, nil)
	if err != nil {
		t.Fatalf("ListActiveEndpoints: %v", err)
	}
	terminal := map[string]bool{}
	for _, id := range ids {
		terminal[id] = true
	}
	for _, r := range rows {
		if terminal[r.RunID] {
			t.Fatalf("终态 Run %s 出现在活跃 Endpoint 列表里：SQL 的终态集合与 domain 不一致", r.RunID)
		}
	}
}
