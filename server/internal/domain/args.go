package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 本文件是 C-8.4（参数词法切分）与 C-8.5（公共参数注入、endpoint_key 规范化）的
// 纯函数实现。**禁止**用 sh -c、strings.Fields 或任何 shell 解释：
// `;`、`|`、`$()`、通配符、`$HOME` 一律原样成为 argv 的字面量。

const (
	// MaxArgs 是单次 argv 的条目上限（C-8.4）。
	MaxArgs = 1000
	// MaxArgBytes 是单个 argv 条目的字节上限（C-8.4）。
	MaxArgBytes = 64 * 1024

	// 公共参数保留字（ARCHITECTURE §5.4.1）。
	ParamURL       = "url"
	ParamModelName = "model_name"
	ParamModelPath = "model_path"
)

// ArgLexError 描述 argv 词法切分失败；Code 取 IssueCode* 中 args 相关的值。
type ArgLexError struct {
	Code    string
	Message string
}

func (e *ArgLexError) Error() string { return e.Message }

// SplitArgs 按 C-8.4 的规则把 args_raw 切成 argv。
//
//	空白（空格/Tab/换行）分隔；连续空白视为一个分隔
//	单引号内全部字面量，无转义
//	双引号内只识别 \" 与 \\ 两种转义，不做变量/命令展开
//	引号外反斜杠按字面量保留
//	"" 产生空串 argv（合法）
//	未闭合引号 / NUL 字节 → ArgLexError
//	argv 总数 ≤ 1000，单参数 ≤ 64 KiB
func SplitArgs(raw string) ([]string, error) {
	if strings.IndexByte(raw, 0) >= 0 {
		return nil, &ArgLexError{Code: IssueCodeArgsLex, Message: "args 含 NUL 字节"}
	}

	argv := make([]string, 0, 8)
	var cur strings.Builder
	started := false
	flush := func() {
		if !started {
			return
		}
		argv = append(argv, cur.String())
		cur.Reset()
		started = false
	}

	runes := []rune(raw)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch ch {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			started = true
			j := i + 1
			for ; j < len(runes) && runes[j] != '\''; j++ {
				cur.WriteRune(runes[j])
			}
			if j >= len(runes) {
				return nil, &ArgLexError{Code: IssueCodeArgsLex, Message: "args 含未闭合的单引号"}
			}
			i = j
		case '"':
			started = true
			j := i + 1
			closed := false
			for ; j < len(runes); j++ {
				c := runes[j]
				if c == '\\' && j+1 < len(runes) && (runes[j+1] == '"' || runes[j+1] == '\\') {
					cur.WriteRune(runes[j+1])
					j++
					continue
				}
				if c == '"' {
					closed = true
					break
				}
				cur.WriteRune(c)
			}
			if !closed {
				return nil, &ArgLexError{Code: IssueCodeArgsLex, Message: "args 含未闭合的双引号"}
			}
			i = j
		default:
			started = true
			cur.WriteRune(ch)
		}
		if cur.Len() > MaxArgBytes {
			return nil, &ArgLexError{Code: IssueCodeArgTooLarge,
				Message: fmt.Sprintf("单个参数超过 %d 字节", MaxArgBytes)}
		}
	}
	flush()

	if len(argv) > MaxArgs {
		return nil, &ArgLexError{Code: IssueCodeArgsTooMany,
			Message: fmt.Sprintf("argv 条目超过 %d 个", MaxArgs)}
	}
	for _, a := range argv {
		if len(a) > MaxArgBytes {
			return nil, &ArgLexError{Code: IssueCodeArgTooLarge,
				Message: fmt.Sprintf("单个参数超过 %d 字节", MaxArgBytes)}
		}
		if strings.IndexByte(a, 0) >= 0 {
			return nil, &ArgLexError{Code: IssueCodeArgsLex, Message: "argv 含 NUL 字节"}
		}
	}
	return argv, nil
}

// PublicParamConflict 返回 args 里与平台注入冲突的公共参数名（C-8.5 第 2 条）。
//
// 判定是**精确匹配**：token == "--url" 或 strings.HasPrefix(token, "--url=")，其余两个同理。
// 只做词法级粗筛，不做语义解析。
func PublicParamConflict(argv []string) []string {
	var hits []string
	for _, name := range []string{ParamURL, ParamModelName, ParamModelPath} {
		flag := "--" + name
		for _, tok := range argv {
			if tok == flag || strings.HasPrefix(tok, flag+"=") {
				hits = append(hits, name)
				break
			}
		}
	}
	return hits
}

// InjectPublicParams 按 url → model_name → model_path 的固定顺序把有值的公共参数
// 追加到 argv 末尾（C-8.5 第 3 条）。空值跳过。返回新切片，不修改入参。
func InjectPublicParams(argv []string, ep Endpoint) []string {
	out := make([]string, 0, len(argv)+3)
	out = append(out, argv...)
	if ep.BaseURL != "" {
		out = append(out, "--"+ParamURL+"="+ep.BaseURL)
	}
	if ep.ModelName != "" {
		out = append(out, "--"+ParamModelName+"="+ep.ModelName)
	}
	if ep.ModelPath != "" {
		out = append(out, "--"+ParamModelPath+"="+ep.ModelPath)
	}
	return out
}

// NormalizeEndpointKey 计算端点互斥键（C-8.5 第 5 条）：
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
