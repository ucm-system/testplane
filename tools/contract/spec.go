package main

// OpenAPI 3.1 契约的读取与"规格侧"校验。
//
// 唯一机器契约是 server/api/openapi.yaml（07 §10）；本文件不重复设计语义，
// 只回答三件事：
//   1. 规格自身是否完整（25 个 operation、成功码、错误体、鉴权、$ref 可解析）；
//   2. 错误体形状是否就是 `Error`（code/message/retryable/trace_id）；
//   3. 实现侧探针要用的"成功状态码 + 响应 schema + Content-Type"从哪里取。
//
// 实现侧（live.go）一律从本文件解析出的 Operation 取期望，不硬编码状态码。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Operation 是一个 method+path 的机器契约摘要。
type Operation struct {
	Method    string // GET / POST ...
	Path      string // 含 {run_id} 等模板参数
	ID        string // operationId
	Security  bool   // 是否需要鉴权（op 级覆盖全局 security）
	Responses map[string]Response
}

// Response 是某个状态码的响应契约。
type Response struct {
	Status       string
	ContentTypes []string // application/json、text/plain、text/event-stream ...
	SchemaName   string   // 指向 components.schemas 的名字，空表示无 schema
	Schema       map[string]any
}

// Spec 是解析后的 OpenAPI 文档。
type Spec struct {
	Path       string
	Raw        map[string]any
	Operations []Operation
	ByID       map[string]Operation

	ErrorSchema   map[string]any
	ErrorRequired []string
	ErrorCodes    []string
}

// LoadSpec 读取并解析 openapi.yaml。
func LoadSpec(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s 不是合法 YAML：%w", path, err)
	}
	spec := &Spec{Path: path, Raw: root, ByID: map[string]Operation{}}

	paths := asMap(root["paths"])
	if paths == nil {
		return nil, fmt.Errorf("%s 没有 paths", path)
	}
	for path, item := range paths {
		itemMap := asMap(item)
		params := collectPathParams(itemMap)
		for method, opAny := range itemMap {
			method = strings.ToLower(method)
			if !isHTTPMethod(method) {
				continue
			}
			opMap := asMap(opAny)
			op := Operation{
				Method:    strings.ToUpper(method),
				Path:      path,
				ID:        asString(opMap["operationId"]),
				Responses: map[string]Response{},
			}
			// op 级 security 覆盖全局；显式空数组表示"无需鉴权"。
			if _, ok := opMap["security"]; ok {
				op.Security = len(asSlice(opMap["security"])) > 0
			} else {
				op.Security = len(asSlice(root["security"])) > 0
			}
			opParams := append(params, collectPathParams(opMap)...)
			_ = opParams // 参数完整性在 staticChecks 里检查

			for status, respAny := range asMap(opMap["responses"]) {
				resp, err := spec.parseResponse(respAny)
				if err != nil {
					return nil, fmt.Errorf("%s %s 的响应 %s 解析失败：%w", op.Method, op.Path, status, err)
				}
				resp.Status = status
				op.Responses[status] = resp
			}
			if op.ID == "" {
				op.ID = strings.ToLower(op.Method) + path
			}
			spec.Operations = append(spec.Operations, op)
			spec.ByID[op.ID] = op
		}
	}
	sort.Slice(spec.Operations, func(i, j int) bool {
		if spec.Operations[i].Path != spec.Operations[j].Path {
			return spec.Operations[i].Path < spec.Operations[j].Path
		}
		return spec.Operations[i].Method < spec.Operations[j].Method
	})

	schemas := asMap(asMap(root["components"])["schemas"])
	if schemas == nil {
		return nil, fmt.Errorf("%s 没有 components.schemas", path)
	}
	spec.ErrorSchema = asMap(schemas["Error"])
	if spec.ErrorSchema == nil {
		return nil, fmt.Errorf("%s 缺少 components.schemas.Error", path)
	}
	req, err := spec.schemaRequired(spec.ErrorSchema)
	if err != nil {
		return nil, fmt.Errorf("解析 Error.required 失败：%w", err)
	}
	spec.ErrorRequired = req
	if ec := asMap(schemas["ErrorCode"]); ec != nil {
		spec.ErrorCodes = asStringSlice(ec["enum"])
	}
	return spec, nil
}

func (s *Spec) parseResponse(v any) (Response, error) {
	resolved, err := s.resolve(v)
	if err != nil {
		return Response{}, err
	}
	m := asMap(resolved)
	out := Response{}
	content := asMap(m["content"])
	for ct, c := range content {
		out.ContentTypes = append(out.ContentTypes, ct)
		schemaAny := asMap(c)["schema"]
		if schemaAny == nil {
			continue
		}
		if ref := asString(asMap(schemaAny)["$ref"]); ref != "" {
			out.SchemaName = ref[strings.LastIndex(ref, "/")+1:]
		}
		resolvedSchema, err := s.resolve(schemaAny)
		if err != nil {
			return out, err
		}
		out.Schema = asMap(resolvedSchema)
	}
	sort.Strings(out.ContentTypes)
	return out, nil
}

// resolve 跟随 `$ref`（只支持文档内引用），返回目标节点。
func (s *Spec) resolve(v any) (any, error) {
	seen := map[string]bool{}
	for {
		m := asMap(v)
		if m == nil {
			return v, nil
		}
		ref := asString(m["$ref"])
		if ref == "" {
			return v, nil
		}
		if seen[ref] {
			return nil, fmt.Errorf("循环 $ref：%s", ref)
		}
		seen[ref] = true
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("不支持外部 $ref：%s", ref)
		}
		var cur any = s.Raw
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			next := asMap(cur)
			if next == nil {
				return nil, fmt.Errorf("$ref 路径不存在：%s", ref)
			}
			value, ok := next[part]
			if !ok {
				return nil, fmt.Errorf("$ref 路径不存在：%s", ref)
			}
			cur = value
		}
		v = cur
	}
}

// schemaRequired 解析 schema 的 required（含 allOf 合并）。
func (s *Spec) schemaRequired(schema any) ([]string, error) {
	resolved, err := s.resolve(schema)
	if err != nil {
		return nil, err
	}
	m := asMap(resolved)
	if m == nil {
		return nil, nil
	}
	req := asStringSlice(m["required"])
	for _, sub := range asSlice(m["allOf"]) {
		subReq, err := s.schemaRequired(sub)
		if err != nil {
			return nil, err
		}
		req = append(req, subReq...)
	}
	return uniqueStrings(req), nil
}

// requireFields 校验收到的 JSON 对象是否包含 schema 的 required 字段。
// 这是"实现侧响应形状"的最小校验：足够抓出"包了一层 data / 拼错字段名"，
// 不做完整 JSON Schema 校验（spec 本身已 lint）。
func (s *Spec) requireFields(schemaName string, raw []byte) []string {
	schemas := asMap(asMap(s.Raw["components"])["schemas"])
	schema := asMap(schemas[schemaName])
	if schema == nil {
		return []string{"spec 里找不到 schema " + schemaName}
	}
	req, err := s.schemaRequired(schema)
	if err != nil {
		return []string{"解析 schema 失败：" + err.Error()}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return []string{"响应不是 JSON 对象：" + err.Error()}
	}
	var missing []string
	for _, field := range req {
		if _, ok := obj[field]; !ok {
			missing = append(missing, field)
		}
	}
	return missing
}

// ---- 规格侧校验 ----------------------------------------------------------

// mvpOperations 是 Lead 定稿的 MVP 端点集（25 个）。
// 用硬编码集合而不是"从 spec 反推"，才能抓住 spec 意外增删端点。
var mvpOperations = map[string]bool{
	"GET /healthz":                                      true,
	"GET /readyz":                                       true,
	"GET /api/v1/meta":                                  true,
	"GET /api/v1/me":                                    true,
	"POST /api/v1/runs":                                 true,
	"GET /api/v1/runs":                                  true,
	"GET /api/v1/runs/{run_id}":                         true,
	"POST /api/v1/runs/{run_id}/cancel":                 true,
	"GET /api/v1/runs/{run_id}/report":                  true,
	"GET /api/v1/runs/{run_id}/report.md":               true,
	"GET /api/v1/runs/{run_id}/events":                  true,
	"GET /api/v1/runs/{run_id}/events/history":          true,
	"GET /api/v1/runs/{run_id}/tasks":                   true,
	"GET /api/v1/runs/{run_id}/manifest":                true,
	"GET /api/v1/runs/{run_id}/deploy":                  true,
	"GET /api/v1/tasks":                                 true,
	"GET /api/v1/tasks/{task_id}":                       true,
	"POST /api/v1/tasks/{task_id}/cancel":               true,
	"GET /api/v1/tasks/{task_id}/attempts":              true,
	"GET /api/v1/tasks/{task_id}/attempts/{idx}/result": true,
	"GET /api/v1/tasks/{task_id}/attempts/{idx}/logs":   true,
	"GET /api/v1/testers":                               true,
	"GET /api/v1/testers/{name}":                        true,
	"GET /api/v1/clusters":                              true,
	"GET /api/v1/queue-status":                          true,
}

// excludedPrefixes/paths 是明确不在 MVP 的端点（票 #3 裁决 + openapi 自述）。
var excludedOperations = []string{
	"POST /api/v1/tasks",
	"POST /api/v1/runs/{run_id}/retry",
	"GET /api/v1/runs/{run_id}/retry-plan",
	"POST /api/v1/tasks/{task_id}/retry",
	"PUT /api/v1/admin/profiles",
	"POST /api/v1/admin/clusters",
	"GET /api/v1/profiles",
	"GET /api/v1/secrets",
	"GET /api/v1/admin/audit",
}

// StaticChecks 返回规格侧的全部检查项。
func (s *Spec) StaticChecks() (checks []check, warnings []string) {
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
	}

	// 1) openapi 版本
	version := asString(s.Raw["openapi"])
	add("spec.openapi_3_1", strings.HasPrefix(version, "3.1"),
		fmt.Sprintf("openapi=%q（期望 3.1.x）", version))

	// 2) operation 数与集合
	add("spec.operation_count", len(s.Operations) == len(mvpOperations),
		fmt.Sprintf("spec 里 %d 个 operation，MVP 定稿 %d 个", len(s.Operations), len(mvpOperations)))

	var actual []string
	for _, op := range s.Operations {
		actual = append(actual, op.Method+" "+op.Path)
	}
	sort.Strings(actual)
	var missing, extra []string
	for want := range mvpOperations {
		found := false
		for _, got := range actual {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	for _, got := range actual {
		if !mvpOperations[got] {
			extra = append(extra, got)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	add("spec.operations_match_mvp", len(missing) == 0 && len(extra) == 0,
		fmt.Sprintf("缺：%v；多：%v", missing, extra))

	// 3) operationId 唯一且非空
	ids := map[string]int{}
	var idIssues []string
	for _, op := range s.Operations {
		if strings.TrimSpace(op.ID) == "" {
			idIssues = append(idIssues, op.Method+" "+op.Path+" 缺 operationId")
			continue
		}
		ids[op.ID]++
	}
	for id, n := range ids {
		if n > 1 {
			idIssues = append(idIssues, fmt.Sprintf("operationId 重复：%s（%d 次）", id, n))
		}
	}
	add("spec.operation_ids", len(idIssues) == 0, firstN(idIssues, 5, "operationId 非空且唯一"))

	// 4) 排除端点不存在
	var presentExcluded []string
	for _, excluded := range excludedOperations {
		for _, got := range actual {
			if got == excluded {
				presentExcluded = append(presentExcluded, got)
			}
		}
	}
	add("spec.excluded_endpoints_absent", len(presentExcluded) == 0,
		firstN(presentExcluded, 5, "MVP 排除的端点均不存在"))

	// 5) 鉴权覆盖
	var securityIssues []string
	for _, op := range s.Operations {
		public := op.Path == "/healthz" || op.Path == "/readyz"
		if public && op.Security {
			securityIssues = append(securityIssues, op.Method+" "+op.Path+" 是运维探针，不应要求鉴权")
		}
		if !public && !op.Security {
			securityIssues = append(securityIssues, op.Method+" "+op.Path+" 缺少 security")
		}
	}
	add("spec.security_coverage", len(securityIssues) == 0,
		firstN(securityIssues, 5, "业务端点全部要求 bearerAuth，运维探针无需鉴权"))

	// 6) 成功响应有定义
	var successIssues []string
	for _, op := range s.Operations {
		var codes []string
		for status := range op.Responses {
			if is2xx(status) {
				codes = append(codes, status)
			}
		}
		if len(codes) == 0 {
			successIssues = append(successIssues, op.Method+" "+op.Path+" 没有 2xx 响应")
		}
	}
	add("spec.success_response_defined", len(successIssues) == 0,
		firstN(successIssues, 5, "每个 operation 至少一个 2xx 响应"))

	// 7) 错误响应形状 = Error（healthz/readyz 的 text/plain 503 豁免）
	var errorShapeIssues []string
	for _, op := range s.Operations {
		public := op.Path == "/healthz" || op.Path == "/readyz"
		for status, resp := range op.Responses {
			if is2xx(status) {
				continue
			}
			if public && hasContentType(resp.ContentTypes, "text/plain") {
				continue
			}
			if !hasContentType(resp.ContentTypes, "application/json") {
				errorShapeIssues = append(errorShapeIssues,
					fmt.Sprintf("%s %s 的 %s 不是 application/json（%v）", op.Method, op.Path, status, resp.ContentTypes))
				continue
			}
			if resp.SchemaName != "Error" {
				errorShapeIssues = append(errorShapeIssues,
					fmt.Sprintf("%s %s 的 %s schema=%q（期望 Error）", op.Method, op.Path, status, resp.SchemaName))
			}
		}
	}
	add("spec.error_responses_use_error_schema", len(errorShapeIssues) == 0,
		firstN(errorShapeIssues, 5, "所有 JSON 错误响应都引用 components.schemas.Error"))

	// 8) Error schema 的必要字段（扁平）
	wantFields := []string{"code", "message", "retryable", "trace_id"}
	var missingFields []string
	for _, field := range wantFields {
		if !containsString(s.ErrorRequired, field) {
			missingFields = append(missingFields, field)
		}
	}
	add("spec.error_schema_required", len(missingFields) == 0,
		fmt.Sprintf("Error.required=%v；缺 %v", s.ErrorRequired, missingFields))
	warnings = append(warnings,
		"注意：openapi 的 Error 是**扁平** {code,message,detail,retryable,trace_id}，不是 {error:{...}}；契约/实现按 spec 断言")

	// 9) path 参数都声明了
	var paramIssues []string
	for _, op := range s.Operations {
		declared := s.declaredPathParams(op)
		for _, placeholder := range pathPlaceholders(op.Path) {
			if !declared[placeholder] {
				paramIssues = append(paramIssues, fmt.Sprintf("%s %s 的 {%s} 没有 parameters 声明", op.Method, op.Path, placeholder))
			}
		}
	}
	add("spec.path_params_declared", len(paramIssues) == 0,
		firstN(paramIssues, 5, "所有 path 参数都有 in:path 声明"))

	// 10) 所有 $ref 可解析
	var refIssues []string
	var walk func(v any, where string)
	walk = func(v any, where string) {
		switch node := v.(type) {
		case map[string]any:
			if ref := asString(node["$ref"]); ref != "" {
				if _, err := s.resolve(node); err != nil {
					refIssues = append(refIssues, where+": "+err.Error())
				}
			}
			for k, child := range node {
				walk(child, where+"/"+k)
			}
		case []any:
			for i, child := range node {
				walk(child, fmt.Sprintf("%s[%d]", where, i))
			}
		}
	}
	walk(s.Raw, "#")
	add("spec.refs_resolvable", len(refIssues) == 0, firstN(refIssues, 5, "所有 $ref 都能解析"))

	// 11) 错误码枚举
	critical := []string{"VALIDATION_ERROR", "UNAUTHENTICATED", "FORBIDDEN", "NOT_FOUND", "CONFLICT", "INTERNAL_ERROR", "PRECHECK_FAILED", "ENDPOINT_UNREACHABLE", "EXECUTOR_UNAVAILABLE", "TASK_TIMEOUT"}
	var missingCodes []string
	for _, code := range critical {
		if !containsString(s.ErrorCodes, code) {
			missingCodes = append(missingCodes, code)
		}
	}
	add("spec.error_codes", len(s.ErrorCodes) >= 20 && len(missingCodes) == 0,
		fmt.Sprintf("ErrorCode 枚举 %d 个；缺关键码 %v", len(s.ErrorCodes), missingCodes))

	return checks, warnings
}

// declaredPathParams 汇总 path item 与 operation 上的 path 参数（支持 $ref）。
func (s *Spec) declaredPathParams(op Operation) map[string]bool {
	out := map[string]bool{}
	paths := asMap(s.Raw["paths"])
	item := asMap(paths[op.Path])
	collect := func(params any) {
		for _, p := range asSlice(params) {
			resolved, err := s.resolve(p)
			if err != nil {
				continue
			}
			pm := asMap(resolved)
			name := asString(pm["name"])
			if asString(pm["in"]) == "path" && name != "" {
				out[name] = true
			}
		}
	}
	collect(item["parameters"])
	collect(asMap(item[opMethodKey(op.Method)])["parameters"])
	return out
}

// ---- 小工具 --------------------------------------------------------------

func pathPlaceholders(path string) []string {
	var out []string
	for {
		start := strings.Index(path, "{")
		if start < 0 {
			break
		}
		end := strings.Index(path[start:], "}")
		if end < 0 {
			break
		}
		out = append(out, path[start+1:start+end])
		path = path[start+end+1:]
	}
	return out
}

func opMethodKey(method string) string { return strings.ToLower(method) }

func isHTTPMethod(m string) bool {
	switch m {
	case "get", "post", "put", "delete", "patch", "head", "options":
		return true
	}
	return false
}

func is2xx(status string) bool {
	return len(status) == 3 && status[0] == '2'
}

func hasContentType(types []string, want string) bool {
	for _, t := range types {
		if strings.HasPrefix(t, want) {
			return true
		}
	}
	return false
}

func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[fmt.Sprint(k)] = v
		}
		return out
	}
	return nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asStringSlice(v any) []string {
	var out []string
	for _, item := range asSlice(v) {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func firstN(items []string, n int, okDetail string) string {
	if len(items) == 0 {
		return okDetail
	}
	if len(items) > n {
		items = append(items[:n], fmt.Sprintf("…共 %d 条", len(items)))
	}
	return strings.Join(items, "；")
}

// collectPathParams 只用于把 path item 级别的 parameters 一起带出来。
func collectPathParams(item map[string]any) []any {
	if item == nil {
		return nil
	}
	return asSlice(item["parameters"])
}

// RepoRoot 从 cwd 向上找 server/api/openapi.yaml，找不到就用默认相对路径。
func DefaultSpecPath() string {
	wd, err := os.Getwd()
	if err != nil {
		return "server/api/openapi.yaml"
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(wd, "server", "api", "openapi.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			break
		}
		wd = parent
	}
	return filepath.Join(wd, "server", "api", "openapi.yaml")
}
