package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Config 是 Authenticator 的构造参数（由 config.Config 投影而来）。
type Config struct {
	Mode         string // dev | oidc
	Issuer       string
	ClientID     string
	ClientSecret string
	ClientAuth   string // basic | post
	RedirectURL  string
	Scopes       []string
	RoleClaim    string
	RoleMap      map[string]string

	SessionSecret []byte
	CookieName    string
	TTL           time.Duration
	Secure        bool

	HTTP *http.Client
	Now  func() time.Time
}

// Authenticator 把 OIDC provider 与签名 Cookie 组合成 API 层要的两个能力：
// 浏览器会话（含 CSRF）与 Bearer JWT。dev 模式下退化为"Bearer <subject> 即身份、角色 admin"。
type Authenticator struct {
	cfg      Config
	provider *Provider
	codec    SessionCodec
	roles    RoleMapper

	CookieName string
	Secure     bool
	TTL        time.Duration
}

// New 构造 Authenticator。oidc 模式缺关键配置返回错误（配置层应已挡住）。
func New(cfg Config) (*Authenticator, error) {
	if cfg.Mode == "" {
		cfg.Mode = "dev"
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "ag_session"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 8 * time.Hour
	}
	if cfg.RoleClaim == "" {
		cfg.RoleClaim = "groups"
	}
	a := &Authenticator{
		cfg:        cfg,
		codec:      SessionCodec{Secret: cfg.SessionSecret, Now: cfg.Now},
		roles:      RoleMapper{Claim: cfg.RoleClaim, Map: cfg.RoleMap},
		CookieName: cfg.CookieName,
		Secure:     cfg.Secure,
		TTL:        cfg.TTL,
	}
	if cfg.Mode == "oidc" {
		if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" || len(cfg.SessionSecret) < 32 {
			return nil, errors.New("auth: oidc 模式缺少 issuer/client_id/redirect_url/session_secret(≥32B)")
		}
		a.provider = &Provider{
			Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			ClientAuth: cfg.ClientAuth, RedirectURL: cfg.RedirectURL, Scopes: cfg.Scopes,
			HTTP: cfg.HTTP, Now: cfg.Now,
		}
	}
	return a, nil
}

// Mode 返回鉴权模式。
func (a *Authenticator) Mode() string { return a.cfg.Mode }

// Begin 生成一次授权跳转：返回 IdP 地址与需要设置的中间态 Cookie。
func (a *Authenticator) Begin(ctx context.Context, returnTo string) (redirect string, cookie *http.Cookie, err error) {
	if a.provider == nil {
		return "", nil, errors.New("auth: dev 模式不支持 OIDC 登录")
	}
	state, err := randomToken()
	if err != nil {
		return "", nil, err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", nil, err
	}
	verifier, err := randomToken()
	if err != nil {
		return "", nil, err
	}
	tx := TxState{
		State: state, Nonce: nonce, Verifier: verifier,
		ReturnTo: safeReturn(returnTo), Exp: a.now().Add(txTTL).Unix(),
	}
	sealed, err := a.codec.Seal(tx)
	if err != nil {
		return "", nil, err
	}
	redirect, err = a.provider.LoginURL(ctx, state, nonce, PKCEChallenge(verifier))
	if err != nil {
		return "", nil, err
	}
	return redirect, a.txCookie(sealed, int(txTTL.Seconds())), nil
}

// Complete 处理回调：校验 state → 换 token → 校验 nonce → 生成会话 Cookie。
func (a *Authenticator) Complete(ctx context.Context, r *http.Request, code, state string) (*http.Cookie, string, error) {
	if a.provider == nil {
		return nil, "/", errors.New("auth: dev 模式不支持 OIDC 回调")
	}
	c, err := r.Cookie(txCookieName)
	if err != nil {
		return nil, "/", fmt.Errorf("%w: 缺少登录中间态 Cookie", ErrUnauthenticated)
	}
	var tx TxState
	if err := a.codec.Open(c.Value, &tx); err != nil {
		return nil, "/", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if a.now().Unix() > tx.Exp {
		return nil, "/", fmt.Errorf("%w: 登录中间态已过期", ErrUnauthenticated)
	}
	if state == "" || state != tx.State {
		return nil, "/", fmt.Errorf("%w: state 不匹配", ErrUnauthenticated)
	}
	claims, raw, err := a.provider.Exchange(ctx, code, tx.Verifier, tx.Nonce)
	if err != nil {
		return nil, "/", err
	}
	groups := groupsFrom(raw, a.roles.Claim)
	if len(groups) == 0 {
		groups = claims.Groups
	}
	sess := Session{
		Sub: claims.Subject, Email: claims.Email,
		Role: a.roles.RoleFor(groups), Groups: groups,
		Exp: a.now().Add(a.TTL).Unix(),
	}
	sess.CSRF = a.codec.CSRFToken(sess)
	sealed, err := a.codec.Seal(sess)
	if err != nil {
		return nil, "/", err
	}
	return a.sessionCookie(sealed, int(a.TTL.Seconds())), tx.ReturnTo, nil
}

// LogoutCookie 返回清除会话的 Cookie。
func (a *Authenticator) LogoutCookie() *http.Cookie {
	return a.sessionCookie("", -1)
}

// TxClearCookie 返回清除登录中间态的 Cookie。
func (a *Authenticator) TxClearCookie() *http.Cookie {
	return a.txCookie("", -1)
}

// FromRequest 解析请求身份：优先会话 Cookie（浏览器），其次 Bearer JWT（服务间）。
// 两者都失败 → ErrUnauthenticated。dev 模式下退化为 `Bearer <subject>` 即 admin。
func (a *Authenticator) FromRequest(r *http.Request) (*Identity, error) {
	now := a.now()
	if a.cfg.Mode == "dev" {
		user := "dev"
		if h := r.Header.Get("Authorization"); h != "" {
			parts := strings.SplitN(h, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				return nil, fmt.Errorf("%w: Authorization 头格式非法", ErrUnauthenticated)
			}
			if sub := strings.TrimSpace(parts[1]); sub != "" {
				user = sub
			}
		}
		return &Identity{User: user, Role: RoleAdmin, Via: "dev"}, nil
	}

	// 1) 会话 Cookie
	if c, err := r.Cookie(a.CookieName); err == nil && c.Value != "" {
		var s Session
		if err := a.codec.Open(c.Value, &s); err == nil && s.Valid(now) && s.Sub != "" {
			return &Identity{
				User: s.Sub, Email: s.Email, Role: s.Role,
				Groups: s.Groups, Via: "cookie",
			}, nil
		}
	}
	// 2) Bearer JWT
	if h := r.Header.Get("Authorization"); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, fmt.Errorf("%w: Authorization 头格式非法", ErrUnauthenticated)
		}
		claims, raw, err := a.provider.VerifyJWT(r.Context(), strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, err
		}
		groups := groupsFrom(raw, a.roles.Claim)
		if len(groups) == 0 {
			groups = claims.Groups
		}
		return &Identity{
			User: claims.Subject, Email: claims.Email,
			Role: a.roles.RoleFor(groups), Groups: groups, Via: "bearer",
		}, nil
	}
	return nil, fmt.Errorf("%w: 缺少会话或承载令牌", ErrUnauthenticated)
}

// CSRFFor 返回该请求会话对应的 CSRF token（供 GET /me 下发）。
func (a *Authenticator) CSRFFor(r *http.Request) (string, bool) {
	if a.cfg.Mode == "dev" {
		return "", false
	}
	c, err := r.Cookie(a.CookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	var s Session
	if err := a.codec.Open(c.Value, &s); err != nil || !s.Valid(a.now()) {
		return "", false
	}
	return a.codec.CSRFToken(s), true
}

// CheckCSRF 校验写操作的 `X-CSRF-Token`（双提交）。Bearer 调用天然免疫 CSRF，跳过。
func (a *Authenticator) CheckCSRF(r *http.Request, id *Identity) bool {
	if id == nil || id.Via != "cookie" {
		return true
	}
	want, ok := a.CSRFFor(r)
	if !ok || want == "" {
		return false
	}
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.Header.Get("X-Csrf-Token")
	}
	return subtleEqual(got, want)
}

func (a *Authenticator) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}

// NewStateToken 生成随机串（供需要 state 的调用方复用，比如测试）。
func NewStateToken() (string, error) { return randomToken() }

// randomHex 是 16 字节十六进制随机串（内部工具）。
func randomHex() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "0"
	}
	return hex.EncodeToString(buf)
}
