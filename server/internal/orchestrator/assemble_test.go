package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// fakeCatalog 是 Assemble 的假注册表（不落库）。
type fakeCatalog struct {
	specs  map[string]domain.RunnerSpec
	listed map[string][]domain.RunnerSpec
}

func (f *fakeCatalog) Resolve(_ context.Context, name, version string) (domain.RunnerSpec, error) {
	if version == "" {
		// 最高版本（这里简化为 map 里该 name 的任意一条）。
		for k, v := range f.specs {
			if strings.HasPrefix(k, name+"@") {
				return v, nil
			}
		}
		return domain.RunnerSpec{}, domain.NewFailureError(domain.CodeRunnerNotFound, "runner not found: "+name, nil)
	}
	if v, ok := f.specs[name+"@"+version]; ok {
		return v, nil
	}
	return domain.RunnerSpec{}, domain.NewFailureError(domain.CodeRunnerNotFound, "runner not found: "+name+"@"+version, nil)
}

func (f *fakeCatalog) List(_ context.Context, role string, _ bool) ([]domain.RunnerSpec, error) {
	return f.listed[role], nil
}

func testSpec(name, version, role string) domain.RunnerSpec {
	return domain.RunnerSpec{
		Name: name, Version: version, Role: role,
		OnFailure:   domain.OnFailureContinue,
		Entry:       []string{"/runner/run.sh"},
		InputFormat: domain.InputFormatJSONL,
		Image:       "registry/argus/" + name + ":" + version,
	}
}

func builtin(name string) domain.RunnerSpec {
	s := testSpec(name, "1.0.0", "test")
	switch name {
	case InjectedDeployName:
		s.Role = domain.RoleDeploy
		s.InputFormat = domain.InputFormatYAML
	case InjectedCleanName:
		s.Role = domain.RoleClean
	}
	return s
}

func testAssembler(c Catalog) *Assembler {
	return &Assembler{
		Catalog: c,
		ProfileExists: func(_ context.Context, name string) (bool, error) {
			return name != "missing", nil
		},
		SecretExists: func(_ context.Context, name string) (bool, error) {
			return name != "missing/secret", nil
		},
		DefaultTimeoutSeconds: 3600,
		MaxTimeoutSeconds:     86400,
		MaxRunners:            200,
	}
}

func baseManifest() *Manifest {
	return &Manifest{Task: TaskBody{
		Name:    "demo",
		Service: &ServiceManifest{URL: "http://10.0.0.10:8000/v1", ModelName: "Qwen3-32B"},
		Runners: []RunnerInput{
			{Name: "acc", Runner: "aisbench-acc", Version: "1.0.0", Execution: ExecutionSpec{Profile: "k8s-npu"},
				Input: `{"dataset":"/data/a"}`},
		},
	}}
}

func catalogWithBuiltin() *fakeCatalog {
	c := &fakeCatalog{specs: map[string]domain.RunnerSpec{}, listed: map[string][]domain.RunnerSpec{}}
	c.specs[InjectedDeployName+"@1.0.0"] = builtin(InjectedDeployName)
	c.specs[InjectedCleanName+"@1.0.0"] = builtin(InjectedCleanName)
	c.specs["aisbench-acc@1.0.0"] = testSpec("aisbench-acc", "1.0.0", "test")
	// precheck: auto 要求注册表恰好一个 role=pre_check（C-5.1 第 11 条）。
	pc := testSpec("endpoint-probe", "1.0.0", domain.RolePreCheck)
	pc.OnFailure = domain.OnFailureAbortTask
	c.specs["endpoint-probe@1.0.0"] = pc
	c.listed[domain.RolePreCheck] = []domain.RunnerSpec{pc}
	return c
}

func TestAssembleExternalModeInjectsNoDeploy(t *testing.T) {
	c := catalogWithBuiltin()
	p, err := testAssembler(c).Assemble(context.Background(), baseManifest(), "t-20261110-AAAA", "dev", nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	for _, rr := range p.Runners {
		if rr.Role == domain.RoleDeploy || rr.Role == domain.RoleClean {
			t.Fatalf("外接模式不应注入 %s", rr.Role)
		}
	}
	if p.Endpoint == nil || p.Endpoint.Key != "http://10.0.0.10:8000" {
		t.Fatalf("endpoint_key 规范化错误: %+v", p.Endpoint)
	}
	for _, rr := range p.Runners {
		if rr.EndpointKey == nil || *rr.EndpointKey != "http://10.0.0.10:8000" {
			t.Fatalf("runner %s 的 endpoint_key 未回填", rr.Name)
		}
	}
}

func TestAssembleDeployModeInjectsDeployAndClean(t *testing.T) {
	c := catalogWithBuiltin()
	m := &Manifest{Task: TaskBody{
		Name:   "deployed",
		Deploy: &DeployManifest{Provider: "genet", Spec: "model: Qwen3-32B\n"},
		Runners: []RunnerInput{
			{Name: "acc", Runner: "aisbench-acc", Version: "1.0.0", Execution: ExecutionSpec{Profile: "k8s-npu"}},
		},
	}}
	p, err := testAssembler(c).Assemble(context.Background(), m, "t-20261110-BBBB", "dev", nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if p.Runners[0].Name != InjectedDeployName || p.Runners[0].Role != domain.RoleDeploy {
		t.Fatalf("seq=1 应为 service-deploy: %+v", p.Runners[0])
	}
	last := p.Runners[len(p.Runners)-1]
	if last.Name != InjectedCleanName || last.Role != domain.RoleClean {
		t.Fatalf("最后一行应为 service-clean: %+v", last)
	}
	if p.DeployDoc == nil || p.DeployDoc.ReleaseName == "" {
		t.Fatalf("部署模式应冻结 DeployDoc: %+v", p.DeployDoc)
	}
	for i, rr := range p.Runners {
		if rr.Seq != i+1 {
			t.Fatalf("seq 不连续: runners[%d].seq=%d", i, rr.Seq)
		}
		if rr.EndpointKey != nil {
			t.Fatalf("部署模式不应有 endpoint_key: %s", rr.Name)
		}
	}
}

func TestAssemblePrecheckAutoInjectsFirst(t *testing.T) {
	c := catalogWithBuiltin()
	pc := testSpec("endpoint-probe", "1.0.0", domain.RolePreCheck)
	pc.OnFailure = domain.OnFailureAbortTask
	c.listed[domain.RolePreCheck] = []domain.RunnerSpec{pc}
	c.specs["endpoint-probe@1.0.0"] = pc

	p, err := testAssembler(c).Assemble(context.Background(), baseManifest(), "t-20261110-CCCC", "dev", nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(p.Runners) != 2 || p.Runners[0].Name != InjectedPrecheckName {
		t.Fatalf("precheck 应注入为第一行: %+v", p.Runners)
	}
	if p.Runners[0].OnFailure != domain.OnFailureAbortTask {
		t.Fatalf("pre_check 必须 abort_task: %s", p.Runners[0].OnFailure)
	}
	if !p.Resolved.Precheck.Injected || p.Resolved.Precheck.Runner != "endpoint-probe" {
		t.Fatalf("resolved.precheck 错误: %+v", p.Resolved.Precheck)
	}
}

func TestAssemblePrecheckOffSkipsInjection(t *testing.T) {
	c := catalogWithBuiltin()
	m := baseManifest()
	m.Task.Precheck = json.RawMessage(`"off"`)
	p, err := testAssembler(c).Assemble(context.Background(), m, "t-20261110-DDDD", "dev", nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if p.Resolved.Precheck.Injected {
		t.Fatalf("precheck=off 不应注入")
	}
}

func TestAssembleValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Manifest)
		code string // issue code
		path string
	}{
		{"deploy_xor_service_both", func(m *Manifest) {
			m.Task.Deploy = &DeployManifest{Provider: "genet"}
		}, domain.IssueCodeDeployService, "task"},
		{"deploy_xor_service_neither", func(m *Manifest) {
			m.Task.Service = nil
		}, domain.IssueCodeDeployService, "task"},
		{"no_runners", func(m *Manifest) {
			m.Task.Runners = nil
		}, domain.IssueCodeRequired, "task.runners"},
		{"injected_name_conflict", func(m *Manifest) {
			m.Task.Runners[0].Name = InjectedCleanName
		}, domain.IssueCodeNameConflict, "runners[0].name"},
		{"profile_missing", func(m *Manifest) {
			m.Task.Runners[0].Execution.Profile = "missing"
		}, "EXECUTION_PROFILE_NOT_FOUND", ""},
		{"on_failure_weakened", func(m *Manifest) {
			m.Task.Runners[0].Runner = "endpoint-probe"
			m.Task.Runners[0].OnFailure = domain.OnFailureContinue
		}, domain.IssueCodeOnFailure, ""},
		{"consumes_unclosed", func(m *Manifest) {
			c := m.Task.Runners
			c[0].Runner = "consumes-runner"
		}, domain.IssueCodeConsumeOpen, ""},
		{"timeout_out_of_range", func(m *Manifest) {
			m.Task.Runners[0].TimeoutSeconds = 86401
		}, domain.IssueCodeTimeoutRange, ""},
		{"input_syntax", func(m *Manifest) {
			m.Task.Runners[0].Input = "{not json"
		}, domain.IssueCodeInputSyntax, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := catalogWithBuiltin()
			c.specs["consumes-runner@1.0.0"] = func() domain.RunnerSpec {
				s := testSpec("consumes-runner", "1.0.0", "test")
				s.Consumes = []string{"AG_DEPLOY_REF"} // 外接模式没有部署导出
				return s
			}()
			c.specs["endpoint-probe@1.0.0"] = testSpec("endpoint-probe", "1.0.0", domain.RolePreCheck)
			m := baseManifest()
			tc.mut(m)
			_, err := testAssembler(c).Assemble(context.Background(), m, "t-20261110-EEEE", "dev", nil)
			if err == nil {
				t.Fatalf("期望校验失败")
			}
			var ve *domain.ValidationError
			var fe *domain.FailureError
			switch {
			case errAs(err, &ve):
				found := false
				for _, is := range ve.Issues {
					if is.Code == tc.code && (tc.path == "" || is.Path == tc.path) {
						found = true
					}
				}
				if !found {
					t.Fatalf("issue 不匹配: want code=%s path=%s got %+v", tc.code, tc.path, ve.Issues)
				}
			case errAs(err, &fe):
				if fe.Failure.Code != tc.code {
					t.Fatalf("failure code 不匹配: want %s got %s", tc.code, fe.Failure.Code)
				}
			default:
				t.Fatalf("错误类型不符: %T %v", err, err)
			}
		})
	}
}

func TestAssembleExportKeyDup(t *testing.T) {
	c := catalogWithBuiltin()
	c.specs["dup-a@1.0.0"] = func() domain.RunnerSpec {
		s := testSpec("dup-a", "1.0.0", "test")
		s.Exports = []string{"SHARED_KEY"}
		return s
	}()
	c.specs["dup-b@1.0.0"] = func() domain.RunnerSpec {
		s := testSpec("dup-b", "1.0.0", "test")
		s.Exports = []string{"SHARED_KEY"}
		return s
	}()
	m := baseManifest()
	m.Task.Runners = []RunnerInput{
		{Name: "a", Runner: "dup-a", Version: "1.0.0", Execution: ExecutionSpec{Profile: "k8s-npu"}},
		{Name: "b", Runner: "dup-b", Version: "1.0.0", Execution: ExecutionSpec{Profile: "k8s-npu"}},
	}
	_, err := testAssembler(c).Assemble(context.Background(), m, "t-20261110-FFFF", "dev", nil)
	var ve *domain.ValidationError
	if !errAs(err, &ve) {
		t.Fatalf("期望 ValidationError: %v", err)
	}
	found := false
	for _, is := range ve.Issues {
		if is.Code == domain.IssueCodeExportDupKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少 EXPORT_KEY_DUP issue: %+v", ve.Issues)
	}
}

func TestAssembleConsumesClosureViaUpstreamExport(t *testing.T) {
	c := catalogWithBuiltin()
	c.specs["producer@1.0.0"] = func() domain.RunnerSpec {
		s := testSpec("producer", "1.0.0", "test")
		s.Exports = []string{"DATASET_REF"}
		return s
	}()
	c.specs["consumer@1.0.0"] = func() domain.RunnerSpec {
		s := testSpec("consumer", "1.0.0", "test")
		s.Consumes = []string{"DATASET_REF"}
		return s
	}()
	m := baseManifest()
	m.Task.Runners = []RunnerInput{
		{Name: "p", Runner: "producer", Version: "1.0.0", Execution: ExecutionSpec{Profile: "k8s-npu"}},
		{Name: "c", Runner: "consumer", Version: "1.0.0", Execution: ExecutionSpec{Profile: "k8s-npu"}},
	}
	if _, err := testAssembler(c).Assemble(context.Background(), m, "t-20261110-GGGG", "dev", nil); err != nil {
		t.Fatalf("上游导出应闭合 consumes: %v", err)
	}
}

func TestParseManifestYAMLAndJSON(t *testing.T) {
	y := []byte("task:\n  name: demo\n  service:\n    url: http://x:8000/v1\n  runners:\n    - name: a\n      runner: r\n      version: 1.0.0\n      execution:\n        profile: p\n")
	m, err := ParseManifest(y)
	if err != nil {
		t.Fatalf("yaml parse: %v", err)
	}
	if m.Task.Name != "demo" || len(m.Task.Runners) != 1 {
		t.Fatalf("yaml 结构不符: %+v", m.Task)
	}
	j := []byte(`{"task":{"name":"demo","service":{"url":"http://x:8000/v1"},"runners":[{"name":"a","runner":"r","version":"1.0.0","execution":{"profile":"p"}}]}}`)
	m2, err := ParseManifest(j)
	if err != nil {
		t.Fatalf("json parse: %v", err)
	}
	if m2.Task.Name != "demo" {
		t.Fatalf("json 结构不符")
	}
}

func TestFailureFromOutcome(t *testing.T) {
	s := &Scheduler{}
	f := s.failureFromOutcome(ports.JobOutcome{Phase: "timeout", Message: "took too long"})
	if f.Code != domain.CodeRunnerTimeout || !f.Retryable {
		t.Fatalf("timeout 归类错误: %+v", f)
	}
	ec := int32(2)
	f = s.failureFromOutcome(ports.JobOutcome{Phase: "failed", Message: "boom", ExitCode: &ec})
	if f.Code != domain.CodeRunnerNonzeroExit {
		t.Fatalf("非零退出应 RUNNER_NONZERO_EXIT: %+v", f)
	}
	f = s.failureFromOutcome(ports.JobOutcome{Phase: "failed", Message: "evicted"})
	if f.Code != domain.CodeInternalError {
		t.Fatalf("无退出码失败应 INTERNAL_ERROR: %+v", f)
	}
}

func errAs(err error, target any) bool {
	return errors.As(err, target)
}
