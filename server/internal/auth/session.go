package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// 浏览器流的中间态（state/nonce/PKCE verifier/回跳路径）放在一张短命签名 Cookie 里，
// 服务端不保存任何登录中间状态——多副本部署时天然一致。
const (
	txCookieName = "tp_auth_tx"
	txTTL        = 10 * time.Minute
)

// TxState 是一次授权请求的中间态。
type TxState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	ReturnTo string `json:"return_to"`
	Exp      int64  `json:"exp"`
}

// SessionCodec 用 HMAC-SHA256 签发/校验 Cookie 载荷（自实现，不引 JWT 库）。
type SessionCodec struct {
	Secret []byte
	Now    func() time.Time
}

func (c SessionCodec) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Seal 把载荷 JSON 签名后编码成 `base64url(payload).base64url(sig)`。
func (c SessionCodec) Seal(payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("auth: 会话序列化失败: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + c.sign(body), nil
}

// Open 校验签名并反序列化（签名不匹配/过期由调用方判断载荷里的 exp）。
func (c SessionCodec) Open(token string, out any) error {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || sig == "" {
		return errors.New("auth: 会话格式非法")
	}
	if !hmac.Equal([]byte(sig), []byte(c.sign(body))) {
		return errors.New("auth: 会话签名无效")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return fmt.Errorf("auth: 会话解码失败: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("auth: 会话解析失败: %w", err)
	}
	return nil
}

func (c SessionCodec) sign(body string) string {
	m := hmac.New(sha256.New, c.Secret)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CSRFToken 由会话绑定字段派生（双提交：Cookie 会话 + `X-CSRF-Token` 头）。
// 用 HMAC(secret, "csrf|sub|exp") 派生，服务端无需存储。
func (c SessionCodec) CSRFToken(s Session) string {
	m := hmac.New(sha256.New, c.Secret)
	m.Write([]byte("csrf|" + s.Sub + "|" + strconvI(s.Exp)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:32]
}

func strconvI(v int64) string { return fmt.Sprintf("%d", v) }

// randomToken 生成 32 字节随机串（state / nonce / PKCE verifier 都用它）。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: 随机数失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// PKCEChallenge 计算 S256 的 code_challenge。
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// allowedReturnPrefixes 是登录后允许回跳的站内路由白名单（07 §2.4）。
// 白名单比"黑名单式校验"更稳：XSS/开放重定向的变体（反斜杠、URL 编码、协议相对）
// 只要归一后不以这些前缀开头，就一律回 "/"。
var allowedReturnPrefixes = []string{
	"/runs", "/tasks", "/submit", "/clusters", "/testers",
	"/profiles", "/queue", "/metrics", "/settings", "/reports",
}

// safeReturn 只允许**站内白名单路由**的相对路径；其它一律 "/"。
//
// 关键：先归一化再判断——`\` 与 `%5C` 在浏览器里等价于 `/`（`/\evil.com` 会被
// 规范化成 `https://evil.com/`），所以必须先把反斜杠换成斜杠、再做解码与解析，
// 否则白名单判断会被绕过（票 #27-1）。
func safeReturn(raw string) string {
	if raw == "" {
		return "/"
	}
	// 1) 反斜杠归一（含 URL 编码形态）：浏览器把 \ 当作路径分隔符。
	norm := strings.ReplaceAll(raw, "\\", "/")
	for strings.Contains(norm, "%5c") || strings.Contains(norm, "%5C") {
		norm = strings.ReplaceAll(norm, "%5c", "/")
		norm = strings.ReplaceAll(norm, "%5C", "/")
	}
	// 2) 其它百分号编码解一次，避免 %2f 之类绕过前缀判断。
	if decoded, err := url.PathUnescape(norm); err == nil {
		norm = strings.ReplaceAll(decoded, "\\", "/")
	}
	// 3) 绝对 URL / 协议相对 / 空 host 一律拒。
	if strings.HasPrefix(norm, "//") {
		return "/"
	}
	u, err := url.Parse(norm)
	if err != nil || u.IsAbs() || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	if !strings.HasPrefix(norm, "/") {
		return "/"
	}
	// 4) 归一化路径（消掉 /a/../b），确保仍以 / 开头；query 保留（如 ?tab=logs）。
	cleanPath := path.Clean(u.Path)
	if !strings.HasPrefix(cleanPath, "/") || strings.HasPrefix(cleanPath, "//") {
		return "/"
	}
	// 5) 白名单前缀（按路径段匹配，避免 /runs-evil 命中 /runs）。
	for _, prefix := range allowedReturnPrefixes {
		if cleanPath == prefix || strings.HasPrefix(cleanPath, prefix+"/") {
			out := cleanPath
			if u.RawQuery != "" {
				out += "?" + u.RawQuery
			}
			return out
		}
	}
	return "/"
}

// Cookie 构造工具（Secure/SameSite/Path 统一在这里，别处不要手写）。
func (a *Authenticator) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     a.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func (a *Authenticator) txCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     txCookieName,
		Value:    value,
		Path:     "/auth",
		HttpOnly: true,
		Secure:   a.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}
