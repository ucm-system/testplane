package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 本文件是 C-8.4（输入校验）与 C-8.5（公共参数注入、endpoint_key 规范化）的
// 纯函数实现。
//
// v2 裁决（D-66/D-67）：业务输入**不走 argv**——平台把提交的输入原文按 Runner 的
// input_format 校验后落盘为输入文件（AG_PARAMS_FILE 注入路径），entry 的 argv 恒空。
// 输入校验只做词法/结构级：jsonl 逐行 JSON.parse + "是对象" + 类型/required 存在性；
// yaml/text 仅语法级。枚举、范围、正则、跨字段约束一律归 Runner（§5.4）。
// 所有函数**无 IO**、不 shell；错误带行号 + 字段名。

const (
	// MaxInputLineBytes 是 jsonl 单行的字节上限（C-8.4 防滥用）。
	MaxInputLineBytes = 1 << 20 // 1 MiB
	// MaxInputLines 是 jsonl 总行数上限（C-8.4 防滥用）。
	MaxInputLines = 100000
)

// InputFileName 返回该 input_format 的输入文件约定名（§5.3：params.jsonl / params.yaml / params.txt）。
func InputFileName(format string) string {
	switch format {
	case InputFormatYAML:
		return "params.yaml"
	case InputFormatText:
		return "params.txt"
	default:
		return "params.jsonl"
	}
}

// jsonTypeOf 返回 JSON 值的类型名（object/array/string/number/integer/boolean/null），
// 供 parameters[].type 的结构校验使用；integer 单独区分（number 且无小数部分）。
func jsonTypeOf(raw json.RawMessage) string {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return ""
}

// ValidateJSONLInput 按 C-8.4 校验 jsonl 输入原文：
//   - 逐行 JSON.parse（错误带行号）；
//   - 校验"是对象"（错误带行号）；
//   - 按 params 校验字段**类型**与 **required 存在性**（错误带行号 + 字段名）；
//   - 未声明的字段**放行**（是否允许归 Runner 决定，§5.4）；
//   - 空行跳过；`#` 开头为注释行跳过；
//   - 单行超 MaxInputLineBytes、总行数超 MaxInputLines → 防滥用拒绝。
func ValidateJSONLInput(raw string, params []Param) []ValidationIssue {
	var issues []ValidationIssue
	add := func(line int, code, field, format string, args ...any) {
		issues = append(issues, ValidationIssue{
			Path: "input", Code: code, Field: field, Line: line,
			Message: fmt.Sprintf(format, args...),
		})
	}
	if raw == "" {
		// 空输入：required 字段校验仍然成立（全部缺失）。
		for _, p := range params {
			if p.Required {
				add(1, IssueCodeRequired, p.Name, "输入为空，缺少必填字段 %q", p.Name)
			}
		}
		return issues
	}

	byName := make(map[string]Param, len(params))
	for _, p := range params {
		byName[p.Name] = p
	}

	lines := strings.Split(raw, "\n")
	present := map[string]bool{}
	count := 0
	for i, line := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		count++
		if count > MaxInputLines {
			add(lineNo, IssueCodeInputTooLarge, "", "行数超过上限 %d", MaxInputLines)
			return issues
		}
		if len(line) > MaxInputLineBytes {
			add(lineNo, IssueCodeInputTooLarge, "", "单行超过 %d 字节", MaxInputLineBytes)
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			add(lineNo, IssueCodeInputSyntax, "", "第 %d 行不是合法 JSON: %s", lineNo, err.Error())
			continue
		}
		for name, val := range obj {
			p, declared := byName[name]
			if !declared {
				continue // 未声明字段放行（§5.4）
			}
			present[name] = true
			got := jsonTypeOf(val)
			if got == "" {
				add(lineNo, IssueCodeInputSyntax, name, "第 %d 行字段 %q 不是合法 JSON 值", lineNo, name)
				continue
			}
			if got == "null" {
				continue // null 视为"未提供"，由 required 校验处理
			}
			// number 接受 integer（jsonl 里 512 是合法的 number）；integer 不接受带小数的 number。
			typeOK := got == p.Type ||
				(p.Type == "number" && got == "integer")
			if !typeOK {
				add(lineNo, IssueCodeTypeMismatch, name,
					"第 %d 行字段 %q 类型为 %s，声明为 %s", lineNo, name, got, p.Type)
			}
		}
	}
	for _, p := range params {
		if p.Required && !present[p.Name] {
			add(1, IssueCodeRequired, p.Name, "缺少必填字段 %q", p.Name)
		}
	}
	return issues
}

// ValidateYAMLInput 按 C-8.4 对 yaml 输入做**语法级**校验（可解析即可；语义完全归 Runner）。
func ValidateYAMLInput(raw string) []ValidationIssue {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var v any
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return []ValidationIssue{{
			Path: "input", Code: IssueCodeInputSyntax,
			Message: "yaml 语法错误: " + err.Error(),
		}}
	}
	return nil
}

// ValidateTextInput 对 text 输入不做校验（§5.4：语义完全归 Runner）。
func ValidateTextInput(raw string) []ValidationIssue { return nil }

// ValidateInput 是提交校验的一步到位入口（C-8.4）：按 input_format 分派。
// yaml/text 时 parameters 必须为 []（由提交校验另行保证，这里不重复判）。
func ValidateInput(format, raw string, params []Param) []ValidationIssue {
	switch format {
	case InputFormatYAML:
		return ValidateYAMLInput(raw)
	case InputFormatText:
		return ValidateTextInput(raw)
	default:
		return ValidateJSONLInput(raw, params)
	}
}

// ---- 公共参数与 endpoint_key（C-8.5）--------------------------------------

// 公共参数保留字对应的环境变量（§5.4.1 / D-66：名称固定，禁止变体）。
const (
	EnvServedURL       = "AG_SERVED_URL"
	EnvServedModelName = "AG_SERVED_MODEL_NAME"
	EnvModelPath       = "AG_MODEL_PATH"
	EnvDeployRef       = "AG_DEPLOY_REF"
	// 元信息 env（附录 B：每 Job 恒注入）。
	EnvTaskID     = "AG_TASK_ID"
	EnvAttempt    = "AG_ATTEMPT"
	EnvRunnerSeq  = "AG_RUNNER_SEQ"
	EnvParamsFile = "AG_PARAMS_FILE"
	EnvOutputDir  = "AG_OUTPUT_DIR"
)

// PublicParamEnvs 返回三个公共参数 env 名（顺序固定）。
func PublicParamEnvs() []string {
	return []string{EnvServedURL, EnvServedModelName, EnvModelPath}
}

// PublicParamsEnv 把 Endpoint 投影成注入 env（只含有值的项；D-27：deploy XOR service）。
func PublicParamsEnv(ep Endpoint) map[string]string {
	out := map[string]string{}
	if ep.BaseURL != "" {
		out[EnvServedURL] = ep.BaseURL
	}
	if ep.ModelName != "" {
		out[EnvServedModelName] = ep.ModelName
	}
	if ep.ModelPath != "" {
		out[EnvModelPath] = ep.ModelPath
	}
	if ep.DeployRef != "" {
		out[EnvDeployRef] = ep.DeployRef
	}
	return out
}

// NormalizeEndpointKey 计算端点互斥键（C-8.5 第 5 条，仅外接模式租约用）：
// 无 scheme 补 http；host 小写；端口补默认（80/443）；key = scheme://host:port；
// 解析失败时退化为 raw:<sha256>，仍保证同一字符串互斥。
func NormalizeEndpointKey(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		sum := sha256.Sum256([]byte(raw))
		return "raw:" + hex.EncodeToString(sum[:])
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return scheme + "://" + host
}

// EndpointFromManifest 把提交里的 service 对象投影成 Endpoint（C-8.5 第 1 条）。
// source 只有 genet 或 service 两种。
func EndpointFromManifest(url, modelName, modelPath string) Endpoint {
	return Endpoint{
		Key:       NormalizeEndpointKey(url),
		BaseURL:   url,
		ModelName: modelName,
		ModelPath: modelPath,
	}
}

// ReleaseName 按 D-44 派生 Genet release 名："arg-" + sha256(task_id:attempt_no)[:12]。
// 必须满足 ^[a-z][a-z0-9-]{0,52}$（arg- 前缀 + 12 个 hex，天然满足）。
func ReleaseName(taskID string, attemptNo int) string {
	sum := sha256.Sum256([]byte(taskID + ":" + strconv.Itoa(attemptNo)))
	return "arg-" + hex.EncodeToString(sum[:])[:12]
}
