package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// pagingRun 造一条 Run（指定提交人、状态、tester、submitted_at），返回 run id。
func pagingRun(t *testing.T, srv *Server, st *store.Store,
	submitter, status, tester string, submittedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	runID := store.NewRunID(submittedAt)
	taskID := store.NewTaskID()
	epKey := "http://paging-ep:8000"
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "paging-" + runID, Kind: "run",
			Manifest:      []byte(`{"run":{"name":"paging"}}`),
			Resolved:      []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: submitter, SubmittedAt: submittedAt,
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "t1", TesterName: tester, TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			Status: string(domain.TaskPending), EndpointKey: &epKey,
			TimeoutSeconds: 60, OnFailure: domain.OnFailureContinue,
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var finished any
	if domain.TerminalRunStatus(domain.RunStatus(status)) {
		finished = time.Now().Add(time.Second)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status=$2, finished_at=$3 WHERE id=$1`,
		runID, status, finished); err != nil {
		t.Fatalf("置状态: %v", err)
	}
	return runID
}

type runListItem struct {
	ID string `json:"id"`
}

type runListBody struct {
	Items      []runListItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
	Total      *int          `json:"total"`
	Page       *int          `json:"page"`
	PageSize   *int          `json:"page_size"`
}

func getRuns(t *testing.T, srv *Server, query string) runListBody {
	t.Helper()
	rec := doGet(t, srv, "/api/v1/runs"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs%s status = %d, body=%s", query, rec.Code, rec.Body.String())
	}
	var body runListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 runs: %v", err)
	}
	return body
}

// TestListRuns_OffsetPaging 是 D-61 的验收①：
// page/page_size 生效、total/page/page_size 正确、跨页不重不漏。
func TestListRuns_OffsetPaging(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()

	who := "page-" + store.NewTaskID()
	base := time.Now().Add(-30 * time.Minute).UTC()
	want := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		want = append(want, pagingRun(t, srv, st, who, string(domain.RunSucceeded),
			"paging-tester", base.Add(time.Duration(i)*time.Minute)))
	}

	// 第 1 页：2 条，total=3，has_more=true。
	first := getRuns(t, srv, "?submitter="+who+"&page=1&page_size=2")
	if len(first.Items) != 2 {
		t.Fatalf("page=1 page_size=2 应回 2 条，实际 %d", len(first.Items))
	}
	if first.Total == nil || *first.Total != 3 {
		t.Fatalf("total = %v, 期望 3", first.Total)
	}
	if first.Page == nil || *first.Page != 1 || first.PageSize == nil || *first.PageSize != 2 {
		t.Fatalf("page/page_size 回显不对: %+v / %+v", first.Page, first.PageSize)
	}
	if !first.HasMore {
		t.Fatal("第 1 页 has_more 应为 true")
	}
	if first.NextCursor != nil {
		t.Fatalf("offset 模式 next_cursor 应为 null，实际 %v", *first.NextCursor)
	}

	// 第 2 页：1 条，has_more=false。
	second := getRuns(t, srv, "?submitter="+who+"&page=2&page_size=2")
	if len(second.Items) != 1 {
		t.Fatalf("page=2 应回 1 条，实际 %d", len(second.Items))
	}
	if second.HasMore {
		t.Fatal("第 2 页 has_more 应为 false")
	}

	// 跨页不重不漏。
	got := map[string]bool{}
	for _, it := range append(append([]runListItem{}, first.Items...), second.Items...) {
		if got[it.ID] {
			t.Fatalf("跨页出现重复 Run: %s", it.ID)
		}
		got[it.ID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("两页合计 %d 条，期望 %d", len(got), len(want))
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("漏了 Run %s", id)
		}
	}

	// 超界页：空 items + 正确 total。
	over := getRuns(t, srv, "?submitter="+who+"&page=9&page_size=2")
	if len(over.Items) != 0 || over.Total == nil || *over.Total != 3 {
		t.Fatalf("超界页应空 items + total=3，实际 items=%d total=%v", len(over.Items), over.Total)
	}

	// 空结果：不存在的 submitter。
	empty := getRuns(t, srv, "?submitter=nobody-"+who+"&page=1&page_size=10")
	if len(empty.Items) != 0 || empty.Total == nil || *empty.Total != 0 {
		t.Fatalf("空结果应 items=0 total=0，实际 %d / %v", len(empty.Items), empty.Total)
	}
}

// TestListRuns_PageOverridesCursor 是验收②：给了 page 就忽略 cursor（互斥），
// 且 offset 响应里 next_cursor=null；纯 cursor 模式 total/page/page_size 都是 null。
func TestListRuns_PageOverridesCursor(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()

	who := "cursor-" + store.NewTaskID()
	base := time.Now().Add(-10 * time.Minute).UTC()
	for i := 0; i < 3; i++ {
		pagingRun(t, srv, st, who, string(domain.RunSucceeded), "paging-tester",
			base.Add(time.Duration(i)*time.Minute))
	}

	// 先拿一个合法 cursor。
	page1 := getRuns(t, srv, "?submitter="+who+"&limit=1")
	if page1.NextCursor == nil {
		t.Fatal("cursor 模式应回 next_cursor")
	}
	// cursor 模式：total/page/page_size 必须为 null。
	if page1.Total != nil || page1.Page != nil || page1.PageSize != nil {
		t.Fatalf("cursor 模式不应带 total/page/page_size: %+v/%+v/%+v",
			page1.Total, page1.Page, page1.PageSize)
	}

	// cursor + page 同时给：page 生效（忽略 cursor），返回 3 条里的前 2 条，不是 cursor 之后的那条。
	mixed := getRuns(t, srv, "?submitter="+who+"&cursor="+*page1.NextCursor+"&page=1&page_size=2")
	if len(mixed.Items) != 2 {
		t.Fatalf("page 生效时应回 2 条，实际 %d", len(mixed.Items))
	}
	if mixed.Total == nil || *mixed.Total != 3 {
		t.Fatalf("page 生效时 total 应为 3，实际 %v", mixed.Total)
	}
	if mixed.NextCursor != nil {
		t.Fatal("offset 模式 next_cursor 必须为 null")
	}
}

// TestListRuns_MultiValueFilters 是验收③：status/submitter 支持逗号多值（IN 语义）。
func TestListRuns_MultiValueFilters(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()

	tag := store.NewTaskID()
	dev, qa := "mv-dev-"+tag, "mv-qa-"+tag
	base := time.Now().Add(-20 * time.Minute).UTC()
	okRun := pagingRun(t, srv, st, dev, string(domain.RunSucceeded), "paging-tester", base)
	failRun := pagingRun(t, srv, st, dev, string(domain.RunFailed), "paging-tester", base.Add(time.Minute))
	qaRun := pagingRun(t, srv, st, qa, string(domain.RunCanceled), "paging-tester", base.Add(2*time.Minute))

	// 多 submitter
	both := getRuns(t, srv, "?submitter="+dev+","+qa+"&page=1&page_size=10")
	if both.Total == nil || *both.Total != 3 {
		t.Fatalf("submitter 多值应命中 3 条，实际 %v", both.Total)
	}
	// 多 status
	multi := getRuns(t, srv, "?submitter="+dev+","+qa+
		"&status="+string(domain.RunSucceeded)+","+string(domain.RunFailed)+"&page=1&page_size=10")
	if multi.Total == nil || *multi.Total != 2 {
		t.Fatalf("status 多值应命中 2 条，实际 %v", multi.Total)
	}
	ids := map[string]bool{}
	for _, it := range multi.Items {
		ids[it.ID] = true
	}
	if !ids[okRun] || !ids[failRun] || ids[qaRun] {
		t.Fatalf("status 多值命中不对: %+v", ids)
	}
}

// TestRunFacets_IndependentCounts 是验收④：facets 计数按维度独立
// （算 status 的计数时应用其它维度过滤、不应用 status 自己）。
func TestRunFacets_IndependentCounts(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()

	tag := store.NewTaskID()
	dev, qa, tester := "fdev-"+tag, "fqa-"+tag, "facet-tester-"+tag
	base := time.Now().Add(-40 * time.Minute).UTC()
	pagingRun(t, srv, st, dev, string(domain.RunSucceeded), tester, base)
	pagingRun(t, srv, st, dev, string(domain.RunFailed), tester, base.Add(time.Minute))
	pagingRun(t, srv, st, qa, string(domain.RunSucceeded), tester, base.Add(2*time.Minute))

	facets := func(query string) map[string]map[string]int {
		t.Helper()
		rec := doGet(t, srv, "/api/v1/runs/facets"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /runs/facets%s status = %d, body=%s", query, rec.Code, rec.Body.String())
		}
		var body struct {
			Status    []store.FacetBucket `json:"status"`
			Submitter []store.FacetBucket `json:"submitter"`
			Kind      []store.FacetBucket `json:"kind"`
			Tester    []store.FacetBucket `json:"tester"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析 facets: %v", err)
		}
		out := map[string]map[string]int{
			"status": {}, "submitter": {}, "kind": {}, "tester": {},
		}
		for _, b := range body.Status {
			out["status"][b.Value] = b.Count
		}
		for _, b := range body.Submitter {
			out["submitter"][b.Value] = b.Count
		}
		for _, b := range body.Kind {
			out["kind"][b.Value] = b.Count
		}
		for _, b := range body.Tester {
			out["tester"][b.Value] = b.Count
		}
		return out
	}

	// 只看本测试造的三条：用 tester 过滤（tester 维度自己不参与 tester 过滤，其余维度都参与）。
	base1 := facets("?tester=" + tester)
	if got := base1["status"][string(domain.RunSucceeded)]; got != 2 {
		t.Fatalf("status= succeeded 计数 = %d, 期望 2（%v）", got, base1["status"])
	}
	if got := base1["status"][string(domain.RunFailed)]; got != 1 {
		t.Fatalf("status=failed 计数 = %d, 期望 1（%v）", got, base1["status"])
	}
	if base1["submitter"][dev] != 2 || base1["submitter"][qa] != 1 {
		t.Fatalf("submitter 计数不对: %v", base1["submitter"])
	}
	if base1["kind"]["run"] != 3 {
		t.Fatalf("kind=run 计数 = %d, 期望 3", base1["kind"]["run"])
	}
	if base1["tester"][tester] != 3 {
		t.Fatalf("tester 维度计数 = %d, 期望 3", base1["tester"][tester])
	}

	// 勾上 submitter=dev：status 维度应只统计 dev 的（1 succeeded + 1 failed），
	// 而 submitter 维度**不应用自己**的过滤 → 仍能看到 qa（1）。
	byDev := facets("?tester=" + tester + "&submitter=" + dev)
	if byDev["status"][string(domain.RunSucceeded)] != 1 || byDev["status"][string(domain.RunFailed)] != 1 {
		t.Fatalf("勾 submitter=dev 后 status 计数不对: %v", byDev["status"])
	}
	if byDev["submitter"][qa] != 1 {
		t.Fatalf("submitter 维度不应被自己的过滤裁掉: %v", byDev["submitter"])
	}

	// 勾上 status=failed：submitter 维度应只剩 dev(1)，status 维度自己仍给全集。
	byFailed := facets("?tester=" + tester + "&status=" + string(domain.RunFailed))
	if byFailed["submitter"][dev] != 1 {
		t.Fatalf("勾 status=failed 后 submitter 计数不对: %v", byFailed["submitter"])
	}
	if byFailed["submitter"][qa] != 0 {
		t.Fatalf("qa 不该出现在 failed 的 submitter 维度: %v", byFailed["submitter"])
	}
	if byFailed["status"][string(domain.RunSucceeded)] != 2 {
		t.Fatalf("status 维度不应被自己的过滤裁掉: %v", byFailed["status"])
	}

	// 空结果：不存在的 submitter 过滤下，status/kind 维度必空；
	// submitter 维度**按 facet 语义不应用自己**（仍列出全部候选），但四个维度都必须存在且是数组。
	emptyRec := doGet(t, srv, "/api/v1/runs/facets?submitter=nobody-"+fmt.Sprint(tag))
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("空 facets status = %d", emptyRec.Code)
	}
	var empty map[string]json.RawMessage
	_ = json.Unmarshal(emptyRec.Body.Bytes(), &empty)
	for _, dim := range []string{"status", "submitter", "kind", "tester"} {
		raw, ok := empty[dim]
		if !ok {
			t.Fatalf("facets 缺维度 %s: %s", dim, emptyRec.Body.String())
		}
		if len(raw) == 0 || raw[0] != '[' {
			t.Fatalf("%s 必须是数组（空时 []，不能是 null）: %s", dim, raw)
		}
	}
	if string(empty["status"]) != "[]" || string(empty["kind"]) != "[]" {
		t.Fatalf("不存在 submitter 过滤下 status/kind 应为 []，实际 status=%s kind=%s",
			empty["status"], empty["kind"])
	}
}
