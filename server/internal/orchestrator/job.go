package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/report"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Job 渲染（C-8.5 / 附录 B spec 2.0）：runner 执行记录 → ports.JobSpec。
//
// env 的注入通道只有一套（D-71）：
//  1. 外接模式：Manifest service 三元组 → AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH
//     （"序号 0 的导出"）；
//  2. 部署模式：service-deploy 成功后收割的 exports 提供同一组 key；
//  3. 任意上游 runner 的 exports（succeeded 且词法合格才收割，C-5.4）；
//  4. Task 级 env（明文）与 secrets（secret_env 引用注入，不落 exports）；
//  5. 元信息 env：AG_TASK_ID / AG_ATTEMPT / AG_RUNNER_SEQ / AG_PARAMS_FILE / AG_OUTPUT_DIR 恒注入。
//
// 冲突处理：按管道线性顺序合并，**后到者不覆盖先到者**（管道序即优先级，C-8.5）。

// 平台元信息 env 的固定键（附录 B：env 全 AG_*）。
const (
	EnvTaskID     = "AG_TASK_ID"
	EnvAttempt    = "AG_ATTEMPT"
	EnvRunnerSeq  = "AG_RUNNER_SEQ"
	EnvParamsFile = "AG_PARAMS_FILE"
	EnvOutputDir  = "AG_OUTPUT_DIR"
)

// 服务公共参数（§5.4.1 固定名，禁止 AG_BASE_URL 变体）。
const (
	EnvServedURL       = "AG_SERVED_URL"
	EnvServedModelName = "AG_SERVED_MODEL_NAME"
	EnvModelPath       = "AG_MODEL_PATH"
	EnvDeployRef       = "AG_DEPLOY_REF"
)

// taskDir 返回 controller 侧可见的 Task 目录：<storage-root>/<task_id>。
func (s *Scheduler) taskDir(taskID string) string {
	root := strings.TrimRight(strings.ReplaceAll(s.cfg.StorageRoot, "\\", "/"), "/")
	return root + "/" + taskID
}

// runnerDir 返回某执行记录的输出目录（宿主视角；$AG_OUTPUT_DIR 同值，§6.8）。
func (s *Scheduler) runnerDir(taskID string, attemptNo, seq int) string {
	return fmt.Sprintf("%s/attempts/%d/runners/%d", s.taskDir(taskID), attemptNo, seq)
}

// relRunnerDir 返回相对 Task 目录的执行记录输出目录（引用落库用）。
func relRunnerDir(attemptNo, seq int) string {
	return fmt.Sprintf("attempts/%d/runners/%d", attemptNo, seq)
}

// refIfFile 报告 <runnerDir>/<name> 真实存在时的落库引用；缺失返回空串（落库 NULL，不造死链）。
func (s *Scheduler) refIfFile(k domain.RunnerKey, name string) string {
	if s.cfg.StorageRoot == "" {
		return ""
	}
	p := s.runnerDir(k.TaskID, k.AttemptNo, k.Seq) + "/" + name
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
		return relRunnerDir(k.AttemptNo, k.Seq) + "/" + name
	}
	return ""
}

func (s *Scheduler) logRefIfFile(k domain.RunnerKey) string {
	return s.refIfFile(k, "logs/"+AttemptLogFileName)
}

// runnerDef 是执行记录的冻结渲染输入（tasks.resolved 的 rows[seq] 子集）。
// 提交时由 assemble 写进 resolved 快照；渲染 Job / 输入文件名都从它读（冻结不回查注册表）。
type runnerDef struct {
	Entry       []string               `json:"entry"`
	InputFormat string                 `json:"input_format"`
	Resources   domain.ResourceRequest `json:"resources"`
	Mounts      []domain.MountSpec     `json:"mounts"`
	// EnvDefaults 是 runner.yaml env 声明的非敏感缺省值（key→default；§12.4）。
	// 渲染时在 Task env / 上游 exports 之后注入（先到者胜，不覆盖显式来源）。
	EnvDefaults map[string]string `json:"env_defaults"`
}

// runnerDefOf 从 Task 的 resolved 快照按 seq 取冻结渲染输入；缺失时回退空值
// （历史 Task 的 resolved 可能没有该子结构）。
func runnerDefOf(t *store.Task, seq int) runnerDef {
	var doc struct {
		Rows []runnerDefRow `json:"runners"`
	}
	_ = json.Unmarshal(t.Resolved, &doc)
	for _, r := range doc.Rows {
		if r.Seq == seq {
			return runnerDef{
				Entry:       r.Entry,
				InputFormat: r.InputFormat,
				Resources:   r.Resources,
				Mounts:      r.Mounts,
				EnvDefaults: r.EnvDefaults,
			}
		}
	}
	return runnerDef{}
}

type runnerDefRow struct {
	Seq         int                    `json:"seq"`
	Entry       []string               `json:"entry,omitempty"`
	InputFormat string                 `json:"input_format,omitempty"`
	Resources   domain.ResourceRequest `json:"resources,omitempty"`
	Mounts      []domain.MountSpec     `json:"mounts,omitempty"`
	EnvDefaults map[string]string      `json:"env_defaults,omitempty"`
}

// buildJobSpec 把执行记录渲染成 ports.JobSpec（附录 B spec 2.0；args 恒空数组，D-66）。
func (s *Scheduler) buildJobSpec(ctx context.Context, rr *store.RunnerRun, jobID string) (ports.JobSpec, error) {
	k := rr.Key()
	t, err := s.st.Tasks.Get(ctx, nil, k.TaskID)
	if err != nil {
		return ports.JobSpec{}, err
	}
	def := runnerDefOf(t, k.Seq)

	// 输入文件：attempts/<n>/input/<seq>/params.<ext>（每 runner 一个子目录、互不可见，E-Q8）。
	inputFile := ""
	inputDir := ""
	if rr.InputRef != "" {
		inputDir = s.taskDir(k.TaskID) + "/" + strings.TrimRight(rr.InputRef, "/")
		inputFile = inputDir + "/" + domain.InputFileName(firstNonEmpty(def.InputFormat, domain.InputFormatJSONL))
	}

	env, secretEnv, err := s.renderRunnerEnv(ctx, t, rr, inputFile)
	if err != nil {
		return ports.JobSpec{}, err
	}

	outputDir := s.runnerDir(k.TaskID, k.AttemptNo, k.Seq)

	labels := map[string]string{
		"argus.io/task":       k.TaskID,
		"argus.io/attempt":    fmt.Sprintf("%d", k.AttemptNo),
		"argus.io/runner-seq": fmt.Sprintf("%d", k.Seq),
	}

	return ports.JobSpec{
		SpecVersion:          ports.JobSpecVersion2,
		JobID:                jobID,
		TaskID:               k.TaskID,
		AttemptNo:            k.AttemptNo,
		Seq:                  k.Seq,
		RunnerName:           rr.RunnerName,
		RunnerVersion:        rr.RunnerVersion,
		Role:                 rr.Role,
		Image:                rr.ImageRef,
		Command:              def.Entry,
		Env:                  env,
		SecretEnv:            secretEnv,
		Namespace:            s.cfg.Namespace,
		Labels:               labels,
		Resources:            def.Resources,
		Mounts:               def.Mounts,
		SharedMounts:         s.effectiveSharedMounts(ctx, rr.Profile),
		SharedMountsDisabled: s.cfg.SharedMountsDisabledProfiles[rr.Profile],
		OutputDir:            outputDir,
		InputDir:             inputDir,
		StoragePVC:           s.cfg.StoragePVC,
		TimeoutSeconds:       rr.TimeoutSeconds,
	}, nil
}

// effectiveSharedMounts 解析本次渲染实际生效的平台级共享挂载（D-65）：
// 优先 DB（SharedMountsFn），失败回落静态种子；不缓存，管理面写入下一次渲染即生效。
func (s *Scheduler) effectiveSharedMounts(ctx context.Context, profile string) []domain.SharedMountSpec {
	shared := s.cfg.SharedMounts
	if s.cfg.SharedMountsFn != nil {
		got, err := s.cfg.SharedMountsFn(ctx)
		if err != nil {
			s.logf("orchestrator: WARN read shared_mounts failed, fallback to seed: %v", err)
		} else {
			shared = got
		}
	}
	if s.cfg.SharedMountsDisabledProfiles[profile] {
		s.logf("orchestrator: profile %s disables unified shared mounts (D-63), not injected", profile)
		return nil
	}
	return shared
}

// renderRunnerEnv 合成本次执行记录的 env（C-8.5 的 5 条来源，先到者胜）。
func (s *Scheduler) renderRunnerEnv(ctx context.Context, t *store.Task, rr *store.RunnerRun, inputFile string) (map[string]string, []ports.SecretEnvRef, error) {
	k := rr.Key()
	env := map[string]string{}
	put := func(key, val string) {
		if _, exists := env[key]; !exists {
			env[key] = val
		}
	}

	// 1. Task 级 env（明文；提交时冻结在 tasks.env）。
	var taskEnv map[string]string
	if len(t.Env) > 0 {
		if err := json.Unmarshal(t.Env, &taskEnv); err != nil {
			return nil, nil, fmt.Errorf("orchestrator: parse task env %s: %w", k.TaskID, err)
		}
	}
	for key, val := range taskEnv {
		put(key, val)
	}

	// 2. "序号 0 的导出"：外接模式静态 service 三元组，部署模式 service-deploy 的收割值。
	//    tasks.endpoint 在提交（service）或收割（deploy）后回填，这里统一投影。
	var ep domain.Endpoint
	if len(t.Endpoint) > 0 {
		_ = json.Unmarshal(t.Endpoint, &ep)
	}
	if ep.BaseURL != "" {
		put(EnvServedURL, ep.BaseURL)
	}
	if ep.ModelName != "" {
		put(EnvServedModelName, ep.ModelName)
	}
	if ep.ModelPath != "" {
		put(EnvModelPath, ep.ModelPath)
	}
	if ep.DeployRef != "" {
		put(EnvDeployRef, ep.DeployRef)
	}

	// 3. 上游 runner 的收割 exports（按 seq 升序读落档文件，先到者胜）。
	if s.cfg.StorageRoot != "" {
		runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, k.TaskID, k.AttemptNo)
		if err != nil {
			return nil, nil, err
		}
		for _, up := range runs {
			if up.Seq >= k.Seq || up.ExportsRef == nil || *up.ExportsRef == "" {
				continue
			}
			raw, rerr := osReadFile(s.taskDir(k.TaskID) + "/" + *up.ExportsRef)
			if rerr != nil {
				continue // 落档缺失不算失败（历史 attempt 只读，重试后新目录自会生成）
			}
			kvs, perr := report.ParseExportsEnv(string(raw))
			if perr != nil {
				continue // 收割时已词法校验过；这里读到脏文件只跳过
			}
			for _, kv := range kvs {
				if _, exists := env[kv.Key]; exists {
					// 后到者不覆盖先到者（管道序即优先级，C-8.5 第 2 条）。
					s.logf("orchestrator: WARN export_override_rejected: key %s from seq %d rejected (already provided by an upstream runner)", kv.Key, up.Seq)
					continue
				}
				env[kv.Key] = kv.Value
			}
		}
	}

	// 4. Secret 引用（不落 exports、不进明文 env；§12.4）。
	var refs []map[string]string
	if len(t.SecretEnv) > 0 {
		if err := json.Unmarshal(t.SecretEnv, &refs); err != nil {
			return nil, nil, fmt.Errorf("orchestrator: parse task secret_env %s: %w", k.TaskID, err)
		}
	}
	var secretEnv []ports.SecretEnvRef
	for _, r := range refs {
		secretEnv = append(secretEnv, ports.SecretEnvRef{Name: r["name"], SecretRef: r["secret_ref"]})
	}

	// 5. 元信息 env（附录 B 恒注入）。
	put(EnvTaskID, k.TaskID)
	put(EnvAttempt, fmt.Sprintf("%d", k.AttemptNo))
	put(EnvRunnerSeq, fmt.Sprintf("%d", k.Seq))
	if inputFile != "" {
		// AG_PARAMS_FILE 必须是**容器内**路径（附录 B：/workspace/input/params.<ext>），
		// inputFile 是 controller 侧 <storage-root>/...（挂载源），不能透传给 runner。
		def := runnerDefOf(t, k.Seq)
		format := firstNonEmpty(def.InputFormat, domain.InputFormatJSONL)
		put(EnvParamsFile, "/workspace/input/"+domain.InputFileName(format))
	}
	put(EnvOutputDir, "/workspace/output")

	// 6. Genet 接入点（§9.1、§12.4）：deploy/clean 解耦后的取值链（先到者胜）——
	//    Task env > 上游 exports > runner.yaml env default（本 runnerDef.EnvDefaults，
	//    如 mock-deploy 的 fake-genet 接入点）> 平台配置（AG_GENET_BASE_URL，兜底）。
	//    平台自身不直连 Genet（D-70），这里只是把接入点透传给 deploy/clean 角色；
	//    平台不感知 "genet"/"mock" 名字——接哪个端点完全由 env 声明与 Task env 决定。
	if rr.Role == domain.RoleDeploy || rr.Role == domain.RoleClean {
		def := runnerDefOf(t, k.Seq)
		for key, val := range def.EnvDefaults {
			put(key, val)
		}
		if s.cfg.GenetBaseURL != "" {
			put("AG_GENET_BASE_URL", s.cfg.GenetBaseURL)
		}
	}

	return env, secretEnv, nil
}

// ---- 文件小工具（temp + rename 原子落盘）----------------------------------

func osReadFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// osWriteFileAtomic 原子写文件：先写 <path>.tmp 再 rename（读者永远看到完整快照）。
func osWriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
