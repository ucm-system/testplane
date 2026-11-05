package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeVolcano 是 VolcanoProvider 的测试替身。
type fakeVolcano struct{ snap VolcanoSnapshot }

func (f *fakeVolcano) VolcanoSnapshot(ctx context.Context) VolcanoSnapshot { return f.snap }

func getQueueStatus(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue-status", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue-status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 queue-status: %v", err)
	}
	return body
}

func volcanoOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	v, ok := body["volcano"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 volcano 块: %+v", body)
	}
	return v
}

// TestQueueStatus_VolcanoDefaultsToUnavailable 没接 Volcano 时必须降级，
// 且绝不把"不可达"渲染成 0 排队（D-46 红线）：available=false + detail 有值。
func TestQueueStatus_VolcanoDefaultsToUnavailable(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()

	v := volcanoOf(t, getQueueStatus(t, srv))
	if v["available"] != false {
		t.Fatalf("available = %v, 期望 false", v["available"])
	}
	if v["detail"] != VolcanoSchedulerUnavailable {
		t.Fatalf("detail = %v, 期望 %s", v["detail"], VolcanoSchedulerUnavailable)
	}
	if q, ok := v["queues"].([]any); !ok || len(q) != 0 {
		t.Fatalf("queues 应为空数组，得到 %v", v["queues"])
	}
}

// TestQueueStatus_VolcanoDetailFourStates 四态都能报，且未知值收敛成 scheduler_unavailable。
func TestQueueStatus_VolcanoDetailFourStates(t *testing.T) {
	cases := []string{
		VolcanoCRDMissing, VolcanoRBACForbidden, VolcanoDisabled, VolcanoSchedulerUnavailable,
	}
	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			srv, _, cleanup := integrationServer(t)
			defer cleanup()
			d := want
			srv.volcano = &fakeVolcano{snap: VolcanoSnapshot{Available: false, Detail: &d}}
			v := volcanoOf(t, getQueueStatus(t, srv))
			if v["detail"] != want {
				t.Fatalf("detail = %v, 期望 %s", v["detail"], want)
			}
		})
	}

	// 未知 detail → scheduler_unavailable（枚举外的值不泄露给前端）。
	srv, _, cleanup := integrationServer(t)
	defer cleanup()
	d := "some_other_failure"
	srv.volcano = &fakeVolcano{snap: VolcanoSnapshot{Available: false, Detail: &d}}
	if v := volcanoOf(t, getQueueStatus(t, srv)); v["detail"] != VolcanoSchedulerUnavailable {
		t.Fatalf("未知 detail 应归一为 %s，得到 %v", VolcanoSchedulerUnavailable, v["detail"])
	}
}

// TestQueueStatus_VolcanoAvailablePassesQueues 可用时 detail 必须为 null，队列透传。
func TestQueueStatus_VolcanoAvailablePassesQueues(t *testing.T) {
	srv, _, cleanup := integrationServer(t)
	defer cleanup()
	state := "Open"
	reason := "Insufficient huawei.com/Ascend910"
	srv.volcano = &fakeVolcano{snap: VolcanoSnapshot{
		Available: true,
		Queues: []VolcanoQueue{{
			Name: "cluster-01/genet-shared", State: &state,
			Capability: map[string]string{"huawei.com/Ascend910": "8"},
			Allocated:  map[string]string{"huawei.com/Ascend910": "3"},
			PendingJobs: 2, LongestWaitSeconds: 540, Reason: &reason,
		}},
	}}
	v := volcanoOf(t, getQueueStatus(t, srv))
	if v["available"] != true {
		t.Fatalf("available = %v", v["available"])
	}
	if v["detail"] != nil {
		t.Fatalf("available=true 时 detail 必须为 null，得到 %v", v["detail"])
	}
	queues, ok := v["queues"].([]any)
	if !ok || len(queues) != 1 {
		t.Fatalf("queues = %v", v["queues"])
	}
	q := queues[0].(map[string]any)
	if q["name"] != "cluster-01/genet-shared" || q["pending_jobs"] != float64(2) {
		t.Fatalf("队列字段异常: %+v", q)
	}
	// 其余两块不受影响。
	body := getQueueStatus(t, srv)
	if _, ok := body["slots"].(map[string]any); !ok {
		t.Fatalf("slots 块缺失: %+v", body)
	}
}
