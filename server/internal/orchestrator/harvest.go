package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/report"
	"github.com/ucm-system/testplane/server/internal/store"
)

// exports 收割流水线（C-5.4，仅 succeeded 的 runner）：
//
//	a. 读 $AG_OUTPUT_DIR/exports.env（= attempts/<n>/runners/<seq>/exports.env）；
//	b. 词法校验（report.ParseExportsEnv）：任何一行不合格 → 整体拒绝收割，runner 判
//	   failed（EXPORT_INVALID）；
//	c. 落库落档：写 attempts/<n>/exports/<seq>.exports.env，exports_ref 由 FinishRunnerRun
//	   与状态推进同事务落库；
//	d. 合并注入：收割到的 KV 按 seq 升序并入"当前导出集合"，渲染 seq+1 的 Job env 时
//	   先到者胜（renderRunnerEnv）。
//
// 失败（退出码 ≠ 0）或 SKIPPED 的 runner 不收割。clean/deploy 角色同样收割
//（service-deploy 的四件套 AG_SERVED_* / AG_DEPLOY_REF 就走这条链路，§5.12）。

// harvestExports 执行一次收割：返回落档引用（相对 Task 目录）与 keys 列表。
// 词法不合格返回 EXPORT_INVALID 的 Failure（调用方据此判 runner 失败）。
func (s *Scheduler) harvestExports(ctx context.Context, k domain.RunnerKey, role, runnerOutDir string) (string, []string, []report.ExportKV, *domain.Failure) {
	path := filepath.Join(runnerOutDir, "exports.env")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 没有 exports 是合法的：不是所有 runner 都导出（纯探测类常常没有）。
			return "", nil, nil, nil
		}
		return "", nil, nil, &domain.Failure{
			Code:      domain.CodeInternalError,
			Message:   fmt.Sprintf("读取 exports.env 失败: %v", err),
			Retryable: false,
			Detail:    map[string]any{"runner_seq": k.Seq},
		}
	}
	kvs, perr := report.ParseExportsEnv(string(raw))
	if perr != nil {
		return "", nil, nil, &domain.Failure{
			Code:      domain.CodeExportInvalid,
			Message:   perr.Error(),
			Retryable: false,
			Detail:    map[string]any{"runner_seq": k.Seq, "file": "exports.env"},
		}
	}

	// 凭据不进 exports（§5.12）：值命中 Task 级 secret 引用的明文时拒绝该行。
	// 平台已知明文仅 Task env（secrets 只存引用、平台无明文可比），这里只做这一处比对。
	if f := s.rejectCredentialLines(ctx, k, kvs); f != nil {
		return "", nil, nil, f
	}

	// 落档：attempts/<n>/exports/<seq>.exports.env（落档后只读）。
	ref := fmt.Sprintf("attempts/%d/exports/%d.exports.env", k.AttemptNo, k.Seq)
	dst := s.taskDir(k.TaskID) + "/" + ref
	var b strings.Builder
	for _, kv := range kvs {
		b.WriteString(kv.Key + "=" + kv.Value + "\n")
	}
	if err := osWriteFileAtomic(dst, []byte(b.String())); err != nil {
		return "", nil, nil, &domain.Failure{
			Code:      domain.CodeInternalError,
			Message:   fmt.Sprintf("exports 落档失败: %v", err),
			Retryable: false,
			Detail:    map[string]any{"runner_seq": k.Seq, "ref": ref},
		}
	}
	keys := report.KVKeys(kvs)
	return ref, keys, kvs, nil
}

// harvestKeys 暴露收割 KV 给调用方（Endpoint 回填用）。
func harvestKeys(kvs []report.ExportKV) []report.ExportKV { return kvs }

// rejectCredentialLines 拒绝把 Task 级 env 里的明文凭据写进 exports（§5.12）。
func (s *Scheduler) rejectCredentialLines(ctx context.Context, k domain.RunnerKey, kvs []report.ExportKV) *domain.Failure {
	t, err := s.st.Tasks.Get(ctx, nil, k.TaskID)
	if err != nil {
		return nil // 读不到 Task 配置时放行：收割校验不应阻塞主流程
	}
	var taskEnv map[string]string
	_ = json.Unmarshal(t.Env, &taskEnv)
	if len(taskEnv) == 0 {
		return nil
	}
	for _, kv := range kvs {
		for envKey, val := range taskEnv {
			if val == "" || kv.Value != val {
				continue
			}
			// 值与某个 Task 级 env 明文完全一致：按凭据处理（除非它本来就是公开参数值）。
			if isPublicParamEnv(envKey) {
				continue
			}
			return &domain.Failure{
				Code:      domain.CodeExportInvalid,
				Message:   fmt.Sprintf("exports.env 的 key %s 的值命中 Task 级环境变量 %s 的明文（凭据不得进入 exports，§5.12）", kv.Key, envKey),
				Retryable: false,
				Detail:    map[string]any{"runner_seq": k.Seq, "key": kv.Key},
			}
		}
	}
	return nil
}

func isPublicParamEnv(key string) bool {
	switch key {
	case EnvServedURL, EnvServedModelName, EnvModelPath, EnvDeployRef,
		EnvTaskID, EnvAttempt, EnvRunnerSeq, EnvParamsFile, EnvOutputDir:
		return true
	}
	return strings.HasPrefix(key, "AG_LOG") || strings.HasPrefix(key, "LOG_")
}

// backfillDeployEndpoint 把 service-deploy 收割的四件套投影成 tasks.Endpoint（03 §4.3：
// Endpoint 的 url 只取 deploy exports 收割值，平台不按角色推导主 URL）。
func (s *Scheduler) backfillDeployEndpoint(ctx context.Context, k domain.RunnerKey, kvs []report.ExportKV) {
	get := func(key string) string {
		for _, kv := range kvs {
			if kv.Key == key {
				return kv.Value
			}
		}
		return ""
	}
	url := get(EnvServedURL)
	if url == "" {
		return
	}
	ep := domain.Endpoint{
		Key:       domain.NormalizeEndpointKey(url),
		BaseURL:   url,
		ModelName: get(EnvServedModelName),
		ModelPath: get(EnvModelPath),
		DeployRef: get(EnvDeployRef),
	}
	raw, err := json.Marshal(ep)
	if err != nil {
		return
	}
	if err := s.st.InTx(ctx, func(tx pgx.Tx) error {
		return s.st.Tasks.SetEndpoint(ctx, tx, k.TaskID, raw)
	}); err != nil {
		s.logf("orchestrator: WARN endpoint backfill %s: %v", k.TaskID, err)
	}
}

// captureRunnerLogs 把容器 stdout/stderr 流写到 <task>/attempts/<n>/runners/<seq>/logs/stdout.log
// （temp + rename 原子落盘）。全程 best effort：失败只 WARN，绝不改状态（多留一个 Job 比丢
// 一份日志便宜）。
func (s *Scheduler) captureRunnerLogs(k domain.RunnerKey, o ports.JobOutcome) {
	if s.cfg.StorageRoot == "" {
		return
	}
	s.logWG.Add(1)
	go func() {
		defer s.logWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), attemptLogTimeout)
		defer cancel()
		h := ports.JobHandle{Namespace: s.cfg.Namespace, JobName: o.Handle.JobName}
		rc, err := s.exec.Logs(ctx, h)
		if err != nil {
			s.logf("orchestrator: WARN runner_logs_failed: %s job %s: %v", k.ID(), h.JobName, err)
			return
		}
		if rc == nil {
			s.logf("orchestrator: WARN runner_logs_failed: %s: executor returned a nil stream", k.ID())
			return
		}
		defer rc.Close()

		logsDir := filepath.Join(s.runnerDir(k.TaskID, k.AttemptNo, k.Seq), "logs")
		if err := os.MkdirAll(logsDir, 0o770); err != nil {
			s.logf("orchestrator: WARN runner_logs_failed: %s: %v", k.ID(), err)
			return
		}
		tmpPath := filepath.Join(logsDir, AttemptLogFileName+".tmp")
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			s.logf("orchestrator: WARN runner_logs_failed: %s: %v", k.ID(), err)
			return
		}
		if _, err := io.Copy(f, rc); err != nil {
			f.Close()
			_ = os.Remove(tmpPath)
			s.logf("orchestrator: WARN runner_logs_failed: %s: %v", k.ID(), err)
			return
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(tmpPath)
			s.logf("orchestrator: WARN runner_logs_failed: %s: %v", k.ID(), err)
			return
		}
		finalPath := filepath.Join(logsDir, AttemptLogFileName)
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			s.logf("orchestrator: WARN runner_logs_failed: %s: %v", k.ID(), err)
			return
		}
		// log_ref 回填（终态已落库，引用事后补；空值不覆盖）。
		if err := s.st.RunnerRuns.SetPhysicalFacts(ctx, nil, k, store.PhysicalFacts{
			LogRef: ptr(relRunnerDir(k.AttemptNo, k.Seq) + "/logs/" + AttemptLogFileName),
		}); err != nil {
			s.logf("orchestrator: WARN log_ref_backfill %s: %v", k.ID(), err)
		}
	}()
}

// WaitLogCapture 等所有后台日志落盘结束（测试与优雅关闭用）。
func (s *Scheduler) WaitLogCapture() { s.logWG.Wait() }

func ptr[T any](v T) *T { return &v }
