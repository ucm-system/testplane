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

// builtinManifest 生成最小可跑的 Manifest：
//
//   - deploy 模式（默认）：有 deploy（Genet 部署）+ precheck auto 等价物 + 一个测试 Task；
//     mock-llm 的地址放在 deploy.parameters.endpoint 里，好让 dev 的假 Genet 指向它。
//   - service 模式：不部署，service.url 直接指向 mock-llm（本地没有 Genet 时用这个）。
//
// precheck 写成显式对象而不是 auto：既固定 tester/version，也把等待窗口压短
// （默认 600s 会让失败注入用例跑十分钟，e2e 必须能快进快出）。
func (a *App) builtinManifest() string {
	name := a.opts.RunName
	if name == "" {
		name = "e2e-" + time.Now().Format("20060102-150405")
	}

	profileLine := ""
	if a.opts.Profile != "" {
		profileLine = fmt.Sprintf("      execution: { profile: %s }\n", a.opts.Profile)
	}

	tests := fmt.Sprintf(`    - name: probe-a
      tester: endpoint-probe
      version: "1.0.0"
%s      args: "--wait_timeout_seconds=10 --retry_interval_seconds=1"
`, profileLine)

	precheck := `  precheck:
    tester: endpoint-probe
    version: "1.0.0"
    args: "--wait_timeout_seconds=60 --retry_interval_seconds=1"
`

	header := fmt.Sprintf("# 由 tools/e2e 生成（mode=%s，%s）\n", a.opts.Mode, time.Now().Format(time.RFC3339))

	if a.opts.Mode == "service" {
		return header + fmt.Sprintf(`run:
  name: %q
  service:
    url: %s
    model_name: %s
%s  cleanup: { policy: never }
  tests:
%s`, name, a.opts.MockURL, a.opts.Model, precheck, tests)
	}

	// 默认 deploy 模式
	return header + fmt.Sprintf(`run:
  name: %q
  deploy:
    provider: %s
    parameters:
      model: %s
      endpoint: %s
%s  cleanup: { policy: always }
  tests:
%s`, name, a.opts.DeployProvider, a.opts.Model, a.opts.MockURL, precheck, tests)
}
