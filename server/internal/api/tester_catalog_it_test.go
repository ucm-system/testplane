package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// testerRow 造一条 testers 表记录（retired = orphaned，不删除）。
func testerRow(name, version string, orphaned bool) *store.Tester {
	now := time.Now().UTC()
	var removed *time.Time
	if orphaned {
		removed = &now
	}
	return &store.Tester{
		Name: name, Version: version,
		DisplayName: name + " " + version, Description: name + " tester",
		Role: domain.RoleTest, OnFailure: domain.OnFailureContinue,
		Entry: "/tester/run.sh", Image: "ghcr.io/x/" + name + ":" + version,
		Resources:   []byte(`{"cpu":"1","memory":"1Gi"}`),
		Concurrency: []byte(`{}`), Mounts: []byte(`[]`), K8s: []byte(`{}`),
		Parameters: []byte(`{}`), EnvDecl: []byte(`{}`),
		SourceFile: "testers/" + name + "/tester.yaml",
		ContentSHA: version + "-sha",
		Orphaned:   orphaned, RemovedAt: removed, RegisteredAt: now, RegisteredBy: "system:registry",
	}
}

// specFor 往 fakeResolver 里注入一条 TesterSpec（List/Resolve 都可见）。
func specFor(t *testing.T, srv *Server, name, version string) {
	t.Helper()
	fr, ok := srv.resolver.(*fakeResolver)
	if !ok {
		t.Fatalf("resolver 不是 fakeResolver: %T", srv.resolver)
	}
	key := name + "@" + version
	if fr.specs == nil {
		fr.specs = map[string]domain.TesterSpec{}
	}
	fr.specs[key] = domain.TesterSpec{
		Name: name, Version: version, Role: domain.RoleTest,
		OnFailure: domain.OnFailureContinue, Entry: "/tester/run.sh",
		Image:     "ghcr.io/x/" + name + ":" + version,
		Resources: domain.ResourceRequest{CPU: "1", Memory: "1Gi"},
	}
}

type testerItem struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Latest   bool   `json:"latest"`
	Retired  bool   `json:"retired"`
	Orphaned bool   `json:"orphaned"`
}

func listTesters(t *testing.T, srv *Server, query string) []testerItem {
	t.Helper()
	rec := doGet(t, srv, "/api/v1/testers"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /testers%s status = %d, body=%s", query, rec.Code, rec.Body.String())
	}
	var body struct {
		Items []testerItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	return body.Items
}

// TestListTesters_DefaultReturnsLatestOnly 是票 #60 的验收①：
// 同名多版本时默认每个 name 只回最新版本，且 latest=true / retired=false。
func TestListTesters_DefaultReturnsLatestOnly(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	// 新名字 catalog-tester：0.1.0 与 0.2.0 都在目录里（未退役）。
	for _, v := range []string{"0.1.0", "0.2.0"} {
		specFor(t, srv, "catalog-tester", v)
		if err := st.Testers.Upsert(ctx, nil, testerRow("catalog-tester", v, false)); err != nil {
			t.Fatalf("upsert %s: %v", v, err)
		}
	}

	items := listTesters(t, srv, "")
	byName := map[string]testerItem{}
	for _, it := range items {
		if _, dup := byName[it.Name]; dup {
			t.Fatalf("默认视图里同一 name 出现了多条: %+v", items)
		}
		byName[it.Name] = it
	}
	got, ok := byName["catalog-tester"]
	if !ok {
		t.Fatalf("默认视图缺 catalog-tester: %+v", items)
	}
	if got.Version != "0.2.0" {
		t.Fatalf("默认视图应回最新版本 0.2.0，实际 %s", got.Version)
	}
	if !got.Latest || got.Retired {
		t.Fatalf("0.2.0 应 latest=true retired=false，实际 %+v", got)
	}
	// 默认视图**不**含 0.1.0。
	for _, it := range items {
		if it.Name == "catalog-tester" && it.Version == "0.1.0" {
			t.Fatalf("默认视图不应包含旧版本: %+v", it)
		}
	}
}

// TestListTesters_IncludeRetiredReturnsAllVersions 是验收②：
// include_retired=true 时回全部版本；退役行 retired=true & latest=false，新版本 latest=true。
func TestListTesters_IncludeRetiredReturnsAllVersions(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	specFor(t, srv, "catalog-retired", "0.1.0")
	specFor(t, srv, "catalog-retired", "0.2.0")
	if err := st.Testers.Upsert(ctx, nil, testerRow("catalog-retired", "0.1.0", false)); err != nil {
		t.Fatalf("upsert 0.1.0: %v", err)
	}
	if err := st.Testers.Upsert(ctx, nil, testerRow("catalog-retired", "0.2.0", false)); err != nil {
		t.Fatalf("upsert 0.2.0: %v", err)
	}
	// 目录里删掉 0.1.0 → registry 只标 orphaned（不删行，§5.6）。
	if err := st.Testers.MarkOrphaned(ctx, nil, "catalog-retired", "0.1.0"); err != nil {
		t.Fatalf("MarkOrphaned: %v", err)
	}

	all := listTesters(t, srv, "?include_retired=true")
	// 真机回归证据：带 retired 标志的响应片段（见 PR/回报）。
	if rec := doGet(t, srv, "/api/v1/testers?include_retired=true"); rec.Code == http.StatusOK {
		t.Logf("GET /api/v1/testers?include_retired=true → %s", rec.Body.String())
	}
	got := map[string]testerItem{}
	for _, it := range all {
		if it.Name == "catalog-retired" {
			got[it.Version] = it
		}
	}
	if len(got) != 2 {
		t.Fatalf("include_retired=true 应回 2 个版本，实际 %+v", got)
	}
	if old, ok := got["0.1.0"]; !ok {
		t.Fatalf("缺历史版本 0.1.0: %+v", got)
	} else if !old.Retired || old.Latest || !old.Orphaned {
		t.Fatalf("0.1.0 应 retired=true latest=false orphaned=true，实际 %+v", old)
	}
	if cur, ok := got["0.2.0"]; !ok {
		t.Fatalf("缺最新版本 0.2.0: %+v", got)
	} else if cur.Retired || !cur.Latest {
		t.Fatalf("0.2.0 应 retired=false latest=true，实际 %+v", cur)
	}

	// 默认视图：退役的 0.1.0 不出现。
	for _, it := range listTesters(t, srv, "") {
		if it.Name == "catalog-retired" && it.Version == "0.1.0" {
			t.Fatalf("默认视图不应出现退役版本: %+v", it)
		}
	}
	// 旧参数 orphaned=true 仍等价（CLI 兼容）。
	n := 0
	for _, it := range listTesters(t, srv, "?orphaned=true") {
		if it.Name == "catalog-retired" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("orphaned=true 应等价于 include_retired=true（2 条），实际 %d", n)
	}
}

// TestTesterVersion_HistoricalReferencePreserved 是验收③（锁历史引用）：
// 引用 0.1.0 的 Task 在升级到 0.2.0 之后，Task 详情仍显示 @0.1.0；
// 而 /testers 指向 0.2.0（latest），0.1.0 只能通过 include_retired 看到且 retired=true。
func TestTesterVersion_HistoricalReferencePreserved(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	// 历史：0.1.0 存在时提交了 Run，Task 冻结 tester=0.1.0。
	specFor(t, srv, "catalog-hist", "0.1.0")
	if err := st.Testers.Upsert(ctx, nil, testerRow("catalog-hist", "0.1.0", false)); err != nil {
		t.Fatalf("upsert 0.1.0: %v", err)
	}
	runID := store.NewRunID(srv.now())
	taskID := store.NewTaskID()
	epKey := "http://hist-ep:8000"
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "hist-run", Kind: "run",
			Manifest:      []byte(`{"run":{"name":"hist-run"}}`),
			Resolved:      []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev", SubmittedAt: srv.now(),
		},
		Tasks: []*store.Task{{
			ID: taskID, Seq: 0, Name: "hist-task", TesterName: "catalog-hist", TesterVersion: "0.1.0",
			ImageRef: "ghcr.io/x/catalog-hist:0.1.0", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			Status: string(domain.TaskSucceeded), CurrentAttempt: 1,
			EndpointKey: &epKey, TimeoutSeconds: 60, OnFailure: domain.OnFailureContinue,
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// 升级：新增 0.2.0，0.1.0 目录消失 → 只标 orphaned，绝不删行。
	specFor(t, srv, "catalog-hist", "0.2.0")
	if err := st.Testers.Upsert(ctx, nil, testerRow("catalog-hist", "0.2.0", false)); err != nil {
		t.Fatalf("upsert 0.2.0: %v", err)
	}
	if err := st.Testers.MarkOrphaned(ctx, nil, "catalog-hist", "0.1.0"); err != nil {
		t.Fatalf("MarkOrphaned: %v", err)
	}

	// 历史 Task 仍按 name@version 解析。
	rec := doGet(t, srv, "/api/v1/tasks/"+taskID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET task status = %d", rec.Code)
	}
	var task struct {
		TesterName    string `json:"tester_name"`
		TesterVersion string `json:"tester_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("解析 task: %v", err)
	}
	if task.TesterName != "catalog-hist" || task.TesterVersion != "0.1.0" {
		t.Fatalf("历史 Task 的 tester = %s@%s, 期望 catalog-hist@0.1.0（版本被改写 = 破坏可追溯性）",
			task.TesterName, task.TesterVersion)
	}

	// Run 的 Task 列表同样显示 @0.1.0。
	rec = doGet(t, srv, "/api/v1/runs/"+runID+"/tasks")
	var listed struct {
		Items []struct {
			TesterVersion string `json:"tester_version"`
		} `json:"items"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run tasks status = %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Items) == 0 || listed.Items[0].TesterVersion != "0.1.0" {
		t.Fatalf("Run 的 Task 列表 tester_version 不是 0.1.0: %+v", listed.Items)
	}

	// 目录默认视图指向 0.2.0；0.1.0 仍可精确查询到（retired=true, latest=false）。
	rec = doGet(t, srv, "/api/v1/testers/catalog-hist")
	var cur testerItem
	if rec.Code != http.StatusOK {
		t.Fatalf("GET tester status = %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cur)
	if cur.Version != "0.2.0" || !cur.Latest || cur.Retired {
		t.Fatalf("目录最新版本应为 0.2.0 latest=true retired=false，实际 %+v", cur)
	}
	rec = doGet(t, srv, "/api/v1/testers/catalog-hist/versions/0.1.0")
	var old testerItem
	if rec.Code != http.StatusOK {
		t.Fatalf("GET tester@0.1.0 status = %d, body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &old)
	if old.Version != "0.1.0" || !old.Retired || old.Latest {
		t.Fatalf("0.1.0 应 retired=true latest=false，实际 %+v", old)
	}
}

// TestVersionLess_Semantics 锁版本比较规则（domain 唯一真源，注册表与 API 共用）。
func TestVersionLess_Semantics(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.2.0", "0.1.0", false},
		{"0.10.0", "0.9.0", false}, // 数字比较，不是字典序
		{"1.0.0", "1.0.0", false},
		{"0.2.0", "0.2.0-rc1", true}, // 同核心版本时按字符串比较（沿用注册表历史行为）
	}
	for _, c := range cases {
		if got := domain.VersionLess(c.a, c.b); got != c.want {
			t.Fatalf("VersionLess(%q,%q) = %v, 期望 %v", c.a, c.b, got, c.want)
		}
	}
}
