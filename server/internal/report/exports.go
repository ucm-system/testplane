package report

import (
	"fmt"
	"strings"
)

// exports.env 词法解析（§5.12 / D-71）。
//
// 产出契约：runner 向 `$AG_OUTPUT_DIR/exports.env` 写 KV 行：
//   - 每行 `KEY=VALUE`（首个 `=` 分隔）；
//   - `#` 开头为注释；空行跳过；
//   - 值不含换行（一行一个 KV）；**不得**出现凭据明文（凭据走 Task 级 secrets）。
//
// 收割规则（C-5.4）：runner 以退出码 0 结束时平台读取该文件；**任何一行词法不合格 →
// 整体拒绝收割**，runner 判失败（EXPORT_INVALID，附录 D 500 / 不可重试）。平台只做
// 词法级 KV 透传，不理解键或值的含义（§13.1）。
//
// 大数据走 `AG_OUTPUT_DIR` 产物文件 + 路径入 exports；平台不设业务长度上限，
// 单行超过 MaxExportLineBytes 视为词法不合格（防滥用）。

// MaxExportLineBytes 是 exports.env 单行的字节上限（防滥用；URL/模型名/路径级小字符串）。
const MaxExportLineBytes = 1 << 20 // 1 MiB

// ParseExportsEnv 把 exports.env 的原文解析成有序 KV。
//
// 返回按文件中出现顺序排列的键值对（注入清单保序需要）；任意一行不合格即整体失败。
// KEY 必须匹配 [A-Za-z_][A-Za-z0-9_]*；重复 key 视为不合格（组合期禁止重复 key 的
// 运行期对偶：同一文件里重复写同一 key 是 Runner 的契约错误）。
func ParseExportsEnv(content string) ([]ExportKV, error) {
	var out []ExportKV
	seen := map[string]int{}
	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(line) > MaxExportLineBytes {
			return nil, fmt.Errorf("exports.env 第 %d 行超过 %d 字节", lineNo, MaxExportLineBytes)
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			return nil, fmt.Errorf("exports.env 第 %d 行不是 KEY=VALUE 形态", lineNo)
		}
		key = strings.TrimRight(key, " \t")
		if !validExportKey(key) {
			return nil, fmt.Errorf("exports.env 第 %d 行 KEY %q 非法（须匹配 [A-Za-z_][A-Za-z0-9_]*）", lineNo, key)
		}
		if strings.ContainsAny(value, "\n\r") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("exports.env 第 %d 行的值含换行/NUL（值不含换行，§5.12）", lineNo)
		}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("exports.env 第 %d 行 KEY %q 与第 %d 行重复（§5.12 组合期禁止重复 key）", lineNo, key, prev)
		}
		seen[key] = lineNo
		out = append(out, ExportKV{Key: key, Value: value, Line: lineNo})
	}
	return out, nil
}

// ExportKV 是 exports.env 的一行。
type ExportKV struct {
	Key   string
	Value string
	Line  int
}

// Map 把有序 KV 转成 map（注入合并用）。
func KVMap(kvs []ExportKV) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value
	}
	return out
}

// Keys 返回有序 key 列表（事件 payload 与注入清单用）。
func KVKeys(kvs []ExportKV) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, kv.Key)
	}
	return out
}

// validExportKey 报告 KEY 是否符合词法。
func validExportKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
			// 首字符与后续都允许
		case r >= '0' && r <= '9':
			if i == 0 {
				return false // 首字符不允许数字
			}
		default:
			return false
		}
	}
	return true
}
