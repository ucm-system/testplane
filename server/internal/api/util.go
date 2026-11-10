package api

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// 请求解析与投影的通用小工具（07 §1 的约定集中在这里）。

// splitCSV 解析多值查询参数（`status=running,queued`，07 §6）。
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// atoiDefault 解析整数查询参数；空/非法返回默认值。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// itoa 整数转字符串（路径拼接用）。
func itoa(v int) string { return strconv.Itoa(v) }

// firstNonEmptyStr 返回第一个非空字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// safePathComponent 拒绝路径穿越（C-7.5：不拼接用户输入中的 ..）。
func safePathComponent(v string) (string, bool) {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, `/\`) {
		return "", false
	}
	return v, true
}

// timeLayoutRFC3339 是响应里时间统一用的格式（UTC，带 Z）。
const timeLayoutRFC3339 = time.RFC3339

// marshalPayload 是事件/审计载荷的统一 marshal（失败给空对象，不冒泡）。
func marshalPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
