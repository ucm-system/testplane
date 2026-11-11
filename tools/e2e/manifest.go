package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"
)

// loadManifest 返回要提交的 Manifest 原文与 Content-Type。
// 优先级：--manifest 文件 > 内置模板（--mode deploy|service）。
func (a *App) loadManifest() ([]byte, string, error) {
	if a.opts.ManifestFile != "" {
		raw, err := os.ReadFile(a.opts.ManifestFile)
		if err != nil {
			return nil, "", &UsageError{Msg: fmt.Sprintf("读取 --manifest 失败：%v", err)}
		}
		raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
		if strings.TrimSpace(string(raw)) == "" {
			return nil, "", &UsageError{Msg: fmt.Sprintf("--manifest 文件是空的：%s", a.opts.ManifestFile)}
		}
		contentType := "application/yaml"
		if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
			contentType = "application/json"
		}
		return raw, contentType, nil
	}
	return []byte(a.builtinManifest()), "application/yaml", nil
}

// builtinManifest 生成最小可跑的 Manifest（v2：顶层嵌套 task:，D-40）：
//
//   - service 模式：不部署，service.url 直接指向 mock-llm（本地没有 Genet 时用这个）；
//   - deploy 模式（默认）：deploy（Genet 部署描述原文，service-deploy runner 消费）+ 一个测试 runner。
//
// precheck 写成显式对象而不是 auto：既固定 runner/version，也把等待窗口压短
// （endpoint-probe 默认 600s 会让失败注入用例跑十分钟，e2e 必须能快进快出）。
// 测试类 runner 用 aisbench-sla 的 stub（aisbench_cmd 参数指向镜像内 stub），秒级返回。
func (a *App) builtinManifest() string {
	name := a.opts.TaskName
	if name == "" {
		name = "e2e-" + time.Now().Format("20060102-150405")
	}

	profileLine := ""
	if a.opts.Profile != "" {
		profileLine = fmt.Sprintf("      execution: { profile: %s }\n", a.opts.Profile)
	}

	runners := fmt.Sprintf(`    - name: probe-a
      runner: %s
      version: %q
%s      input: |
        {"sla":["ttft.avg:100000"],"input_len":128,"output_len":8,"data_num":8,"dataset":"/runner/fixtures/smoke_dataset.jsonl","work_path":"/tmp/aisbench-ws","aisbench_cmd":"/runner/tool/tests/stub_aisbench.py"}
`, a.opts.RunnerName, a.opts.RunnerVersion, profileLine)

	// precheck 块字段缩进是 4 空格（runners[] 内是 6），profile 行不能复用 profileLine。
	precheckProfileLine := ""
	if a.opts.Profile != "" {
		precheckProfileLine = fmt.Sprintf("    execution: { profile: %s }\n", a.opts.Profile)
	}
	precheck := fmt.Sprintf(`  precheck:
    runner: endpoint-probe
    version: %q
%s    input: |
      {"wait_timeout_seconds":10,"retry_interval_seconds":1}
`, a.opts.PrecheckVersion, precheckProfileLine)

	header := fmt.Sprintf("# 由 tools/e2e 生成（mode=%s，%s）\n", a.opts.Mode, time.Now().Format(time.RFC3339))

	if a.opts.Mode == "service" {
		return header + fmt.Sprintf(`task:
  name: %q
  service:
    url: %s
    model_name: %s
    model_path: /models/mock
%s  runners:
%s`, name, a.opts.MockURL, a.opts.Model, precheck, runners)
	}

	// 默认 deploy 模式：deploy.spec 是 Genet 部署描述原文（平台不解析语义，service-deploy 消费）
	return header + fmt.Sprintf(`task:
  name: %q
  deploy:
    provider: %s
    spec: |
      model: %s
      endpoint: %s
%s  runners:
%s`, name, a.opts.DeployProvider, a.opts.Model, a.opts.MockURL, precheck, runners)
}
