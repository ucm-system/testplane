package api

// 测试公共设施（v2 形状）：真 PG + dev 鉴权 + 假 Runner 目录。
// 管理面 / Secret / 共享挂载 / Volcano 的 IT 用例共用；没设 AG_TEST_DATABASE_URL 就 skip。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/store"
)

// adminTestEnv 是管理面测试的最小环境：真 PG + dev 鉴权（Bearer <subject> = admin）。
type adminTestEnv struct {
	t   *testing.T
	srv *Server
	st  *store.Store
	cfg *config.Config
}

var adminNameSeq atomic.Int64

// uniqueAdminName 生成每次测试唯一的对象名（同一测试库跨用例复用，避免撞主键）。
func uniqueAdminName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e9, adminNameSeq.Add(1))
}

func newAdminTestEnv(t *testing.T) *adminTestEnv {
	t.Helper()
	return newAdminTestEnvFor(t, "api")
}

// newAdminTestEnvFor 建在独立测试库上（purpose 派生库名）。
func newAdminTestEnvFor(t *testing.T, purpose string) *adminTestEnv {
	t.Helper()
	dsn, err := dbtest.URL(purpose)
	if err != nil {
		t.Fatalf("准备测试库: %v", err)
	}
	if dsn == "" {
		t.Skip("未设置 AG_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	if err := st.SeedDefaults(ctx); err != nil {
		st.Close()
		t.Fatalf("seed: %v", err)
	}
	cfg := &config.Config{
		APIPrefix:            "/api/v1",
		AuthMode:             "dev",
		StorageRoot:          t.TempDir(),
		RunnerTimeoutDefault: time.Hour,
		RunnerTimeoutMax:     24 * time.Hour,
		TaskMaxRunners:       200,
		AttemptMax:           3,
		MaxGlobalConcurrency: 4,
		DeployTimeout:        72 * time.Hour,
		SSEReplayWindow:      24 * time.Hour,
	}
	srv := New(Deps{Config: cfg, Store: st, Machines: store.NewMachines(st, nil),
		Catalog: itTestCatalog()})
	t.Cleanup(func() { st.Close() })
	return &adminTestEnv{t: t, srv: srv, st: st, cfg: cfg}
}

// do 发一个请求：Bearer <subject> 在 dev 鉴权下即身份（角色 admin）。
func (e *adminTestEnv) do(method, path, body, subject string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if subject == "" {
		subject = "dev"
	}
	req.Header.Set("Authorization", "Bearer "+subject)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON 对象（status=%d）: %v\nbody=%s", rec.Code, err, rec.Body.String())
	}
	return out
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d，期望 %d\nbody=%s", rec.Code, want, rec.Body.String())
	}
	return decodeJSONBody(t, rec)
}

func requireError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	body := requireStatus(t, rec, wantStatus)
	if got, _ := body["code"].(string); got != wantCode {
		t.Fatalf("code = %q，期望 %q\nbody=%s", got, wantCode, rec.Body.String())
	}
	return body
}

func detailOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	d, ok := body["detail"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 detail 对象: %v", body)
	}
	return d
}

// auditRows 读审计行（直接走 store，避免依赖被其它用例污染的列表分页）。
func (e *adminTestEnv) auditRows(filter store.AuditFilter) []store.AuditRecord {
	e.t.Helper()
	rows, _, err := e.st.Audits.List(context.Background(), nil, filter)
	if err != nil {
		e.t.Fatalf("读审计失败: %v", err)
	}
	return rows
}
