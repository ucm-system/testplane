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

// safeReturn 只允许站内相对路径（防开放重定向）；非法值回退到 "/"。
func safeReturn(raw string) string {
	if raw == "" {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") {
		return "/"
	}
	return raw
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
