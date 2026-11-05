package main

// 设计端点目录（design/views/07-api.md §5）的解析，以及“设计有、openapi 无”的
// 已登记缺失白名单。
//
// 背景（PR #13 评审 / 票 #16）：旧实现把 25 个 MVP operation 硬编码后跟 openapi
// 自己比，等于自证；这里改成从设计文档的端点表解析期望集合，再与 openapi 做双向差集。
// 差集里“设计有、openapi 无”的端点必须逐条登记在白名单里并写理由，否则判红。

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	designTableRowRe  = regexp.MustCompile(`^\|`)
	designProseOpRe   = regexp.MustCompile(`(GET|POST|PUT|DELETE|PATCH)\s+(/[A-Za-z0-9_./{}-]+)`)
	designPathParamRe = regexp.MustCompile(`\{[^}]+\}`)
	designMethodRe    = regexp.MustCompile(`^[A-Z]+$`)
)

// DesignEndpoint 是设计文档里的一个 method+path。
type DesignEndpoint struct {
	Method string
	Path   string // 已归一化：带 /api/v1 前缀、模板参数统一为 {}
}

// DesignCatalog 是 §5 解析结果。
type DesignCatalog struct {
	Path      string
	Endpoints []DesignEndpoint
	Set       map[string]bool // key: METHOD path
}

// endpointKey 生成 METHOD path 形式的比较键。
func endpointKey(method, path string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + " " + normalizeEndpointPath(path)
}

// normalizeEndpointPath 把设计/openapi 的 path 归一化到同一坐标系：
//   - 去掉 query / 反引号 / 首尾空白；
//   - 模板参数 {run_id} / {id} / {idx} 统一成 {}；
//   - 非运维探针路径补 /api/v1 前缀。
func normalizeEndpointPath(p string) string {
	p = strings.TrimSpace(strings.Trim(p, "`"))
	if i := strings.IndexAny(p, "?# "); i >= 0 {
		p = p[:i]
	}
	p = designPathParamRe.ReplaceAllString(p, "{}")
	if p == "" {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	switch {
	case p == "/healthz", p == "/readyz", p == "/metrics":
		return p
	case strings.HasPrefix(p, "/api/v1/"):
		return p
	default:
		return "/api/v1" + p
	}
}

// splitMethods 展开 `POST/PUT/DELETE` 这种合并单元格。
func splitMethods(cell string) []string {
	cell = strings.TrimSpace(strings.Trim(cell, "`"))
	if cell == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(cell, "/") {
		m := strings.ToUpper(strings.TrimSpace(part))
		if designMethodRe.MatchString(m) && isHTTPMethod(strings.ToLower(m)) {
			out = append(out, m)
		}
	}
	return out
}

// LoadDesign 解析 07-api.md §5 的端点表 + §5.5 运维段落。
func LoadDesign(path string) (*DesignCatalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取设计文档 %s 失败：%w", path, err)
	}
	cat := &DesignCatalog{Path: path, Set: map[string]bool{}}
	add := func(method, p string) {
		p = normalizeEndpointPath(p)
		if p == "" {
			return
		}
		key := endpointKey(method, p)
		if cat.Set[key] {
			return
		}
		cat.Set[key] = true
		cat.Endpoints = append(cat.Endpoints, DesignEndpoint{Method: strings.ToUpper(method), Path: p})
	}

	inSection := false
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		// §5 的散文端点（`GET /healthz` 等）；表格端点则全文档扫描：
		// §2.4 认证表也定义了 `/me`、`/auth/*`，只解析 §5 会把 `/me` 误判成 spec 多出。
		if strings.HasPrefix(trimmed, "## 5.") {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "## 6.") {
			inSection = false
		}
		if designTableRowRe.MatchString(trimmed) {
			cols := splitMarkdownRow(trimmed)
			if len(cols) < 2 || isTableSeparator(cols[0]) {
				continue
			}
			methods := splitMethods(cols[0])
			if len(methods) == 0 {
				continue
			}
			p := strings.TrimSpace(strings.Trim(cols[1], "`"))
			if !strings.HasPrefix(p, "/") {
				continue
			}
			for _, m := range methods {
				add(m, p)
			}
			continue
		}
		if inSection {
			// §5.5 是散文：`GET /healthz`、`GET /readyz`、`GET /metrics`
			for _, m := range designProseOpRe.FindAllStringSubmatch(line, -1) {
				add(m[1], m[2])
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描设计文档 %s 失败：%w", path, err)
	}
	if len(cat.Endpoints) == 0 {
		return nil, fmt.Errorf("未从 %s §5 解析出任何端点（检查文档结构是否变了）", path)
	}
	sort.Slice(cat.Endpoints, func(i, j int) bool {
		a, b := cat.Endpoints[i], cat.Endpoints[j]
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Path < b.Path
	})
	return cat, nil
}

func splitMarkdownRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func isTableSeparator(cell string) bool {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return false
	}
	for _, r := range cell {
		if r != '-' && r != ':' && r != ' ' {
			return false
		}
	}
	return strings.Contains(cell, "-")
}

// LoadAllowMissing 读取“设计有、openapi 无”的白名单。
// 每行格式：METHOD PATH | 理由。空行与 # 注释忽略；理由缺失/重复/格式非法直接报错。
func LoadAllowMissing(path string) (map[string]string, error) {
	allow := map[string]string{}
	if path == "" || strings.EqualFold(path, "none") {
		return allow, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 allow-missing 白名单 %s 失败：%w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%s:%d 缺少理由（格式 METHOD PATH | 理由）", path, lineNo)
		}
		left := strings.Fields(strings.TrimSpace(parts[0]))
		reason := strings.TrimSpace(parts[1])
		if len(left) != 2 {
			return nil, fmt.Errorf("%s:%d 端点格式应为 METHOD PATH：%q", path, lineNo, parts[0])
		}
		method := strings.ToUpper(left[0])
		if !designMethodRe.MatchString(method) || !isHTTPMethod(strings.ToLower(method)) {
			return nil, fmt.Errorf("%s:%d 非法 HTTP 方法 %q", path, lineNo, left[0])
		}
		if !strings.HasPrefix(left[1], "/") {
			return nil, fmt.Errorf("%s:%d 路径必须以 / 开头：%q", path, lineNo, left[1])
		}
		if reason == "" {
			return nil, fmt.Errorf("%s:%d 理由为空（白名单必须写理由）", path, lineNo)
		}
		key := endpointKey(method, left[1])
		if prev, ok := allow[key]; ok {
			return nil, fmt.Errorf("%s:%d 重复登记 %s（前一条理由：%s）", path, lineNo, key, prev)
		}
		allow[key] = reason
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	return allow, nil
}

// DesignChecks 把“设计端点 ↔ openapi operation”的双向差集变成 checks。
// 白名单只放行“设计有、openapi 无”；openapi 多出来的端点永远判红。
func (s *Spec) DesignChecks(design *DesignCatalog, allow map[string]string) (checks []check, warnings []string) {
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
	}
	if design == nil {
		add("spec.design_endpoints_parsed", false, "没有设计端点目录（未加载 07-api.md §5）")
		add("spec.endpoints_match_design", false, "无法比对：设计端点目录缺失")
		return checks, warnings
	}

	actual := map[string]bool{}
	for _, op := range s.Operations {
		actual[endpointKey(op.Method, op.Path)] = true
	}
	var designOnly, openapiOnly []string
	for key := range design.Set {
		if !actual[key] {
			designOnly = append(designOnly, key)
		}
	}
	for key := range actual {
		if !design.Set[key] {
			openapiOnly = append(openapiOnly, key)
		}
	}
	sort.Strings(designOnly)
	sort.Strings(openapiOnly)

	var unregistered, registered []string
	for _, key := range designOnly {
		if reason, ok := allow[key]; ok {
			registered = append(registered, fmt.Sprintf("%s（%s）", key, reason))
		} else {
			unregistered = append(unregistered, key)
		}
	}
	// 白名单里登记了、但设计里已不存在的端点 → 警告，提示清理。
	var staleAllow []string
	for key := range allow {
		if !design.Set[key] {
			staleAllow = append(staleAllow, key)
		}
	}
	sort.Strings(staleAllow)

	add("spec.design_endpoints_parsed", len(design.Endpoints) > 0,
		fmt.Sprintf("从 %s（§5 端点表 + §2.4 认证表 + §5.5 运维段）解析出 %d 个端点", design.Path, len(design.Endpoints)))

	implementedDesign := len(design.Set) - len(designOnly)
	add("spec.operation_count", len(s.Operations) == implementedDesign,
		fmt.Sprintf("openapi %d 个；设计 %d 个，设计有/实现无 %d 个，期望 %d 个",
			len(s.Operations), len(design.Set), len(designOnly), implementedDesign))

	designPresentOK := len(unregistered) == 0
	designDetail := fmt.Sprintf("设计有、openapi 无 %d 个（已登记 %d，未登记 %d）",
		len(designOnly), len(registered), len(unregistered))
	if len(unregistered) > 0 {
		designDetail += "；未登记：" + firstN(unregistered, 8, "")
	}
	add("spec.design_endpoints_present", designPresentOK, designDetail)

	extraOK := len(openapiOnly) == 0
	add("spec.openapi_endpoints_in_design", extraOK,
		firstN(openapiOnly, 8, "openapi 的 operation 都能在 07-api.md §5 找到"))

	add("spec.endpoints_match_design", designPresentOK && extraOK,
		fmt.Sprintf("设计 %d / openapi %d；设计有实现无 %d（已登记 %d）/ 实现有设计无 %d",
			len(design.Set), len(s.Operations), len(designOnly), len(registered), len(openapiOnly)))

	if len(registered) > 0 {
		warnings = append(warnings,
			fmt.Sprintf("设计有、openapi 无（白名单放行 %d 条）：%s", len(registered), strings.Join(registered, "；")))
	}
	if len(staleAllow) > 0 {
		warnings = append(warnings,
			fmt.Sprintf("allow-missing 中 %d 条已不在设计端点表，请清理：%s", len(staleAllow), strings.Join(staleAllow, ", ")))
	}
	return checks, warnings
}

// DefaultDesignPath 从 cwd 向上找 design/views/07-api.md。
func DefaultDesignPath() string { return findUp("design/views/07-api.md") }

// DefaultAllowMissingPath 从 cwd 向上找 tools/contract/allow-missing.txt。
func DefaultAllowMissingPath() string { return findUp("tools/contract/allow-missing.txt") }

func findUp(rel string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
