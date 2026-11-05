package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider 是 OIDC 服务端的最小客户端：discovery + JWKS 缓存 + token 交换 + JWT 校验。
type Provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	ClientAuth   string // basic | post
	RedirectURL  string
	Scopes       []string
	HTTP         *http.Client
	Now          func() time.Time

	mu          sync.Mutex
	discovery   *discoveryDoc
	discoveryAt time.Time
	keys        map[string]*rsa.PublicKey // kid -> key
	keysAt      time.Time
}

type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint,omitempty"`
}

const (
	discoveryTTL = 10 * time.Minute
	jwksTTL      = 10 * time.Minute
	leeway       = 60 * time.Second
)

// base64RawURL 是 JWT 用的无填充 base64url。
var base64RawURL = base64.RawURLEncoding

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Discover 取并缓存 discovery 文档。
func (p *Provider) Discover(ctx context.Context) (*discoveryDoc, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discoverLocked(ctx)
}

func (p *Provider) discoverLocked(ctx context.Context) (*discoveryDoc, error) {
	if p.discovery != nil && p.now().Sub(p.discoveryAt) < discoveryTTL {
		return p.discovery, nil
	}
	u := strings.TrimRight(p.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: discovery 请求构造失败: %w", err)
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: discovery 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: discovery 返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("auth: discovery 解析失败: %w", err)
	}
	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(p.Issuer, "/") {
		return nil, fmt.Errorf("auth: discovery 的 issuer=%q 与配置 %q 不一致", doc.Issuer, p.Issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, fmt.Errorf("auth: discovery 缺少 authorization/token/jwks 端点")
	}
	p.discovery = &doc
	p.discoveryAt = p.now()
	return p.discovery, nil
}

// keysLocked 取并缓存 JWKS（按 kid 索引）。
func (p *Provider) keysLocked(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	if p.keys != nil && p.now().Sub(p.keysAt) < jwksTTL {
		return p.keys, nil
	}
	doc, err := p.discoverLocked(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, doc.JWKSURI, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: JWKS 请求构造失败: %w", err)
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: JWKS 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: JWKS 返回 %d", resp.StatusCode)
	}
	var set jwks
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("auth: JWKS 解析失败: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pk, err := parseRSAKey(k)
		if err != nil {
			continue
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("auth: JWKS 里没有可用的 RSA 签名 key")
	}
	p.keys = keys
	p.keysAt = p.now()
	return keys, nil
}

// jwtHeader 是 JWS header（只看 alg/kid）。
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// verifyToken 校验签名并把 claims 转成平台结构。raw 用于按 claim 名取 groups。
func (p *Provider) verifyToken(ctx context.Context, token string) (*Claims, rawClaims, jwtClaims, error) {
	var hdr jwtHeader
	raw, c, err := parseClaims(token, leeway, p.now())
	if err != nil {
		return nil, nil, c, err
	}
	if err := json.Unmarshal(mustHeader(token), &hdr); err != nil {
		return nil, nil, c, fmt.Errorf("%w: header 不是 JSON", ErrUnauthenticated)
	}

	p.mu.Lock()
	keys, kerr := p.keysLocked(ctx)
	var pk *rsa.PublicKey
	if kerr == nil {
		pk = keys[hdr.Kid]
		if pk == nil && len(keys) == 1 {
			// 单 key 时不强求 kid 命中（部分 IdP 不填 kid）。
			for _, only := range keys {
				pk = only
			}
		}
	}
	p.mu.Unlock()
	if kerr != nil {
		return nil, nil, c, fmt.Errorf("%w: %v", ErrUnauthenticated, kerr)
	}
	if pk == nil {
		return nil, nil, c, fmt.Errorf("%w: 找不到 kid=%q 的签名 key", ErrUnauthenticated, hdr.Kid)
	}
	if err := verifyRS256(token, pk); err != nil {
		return nil, nil, c, err
	}

	// iss 严格匹配；aud 必须含 clientID
	if strings.TrimRight(c.Issuer, "/") != strings.TrimRight(p.Issuer, "/") {
		return nil, nil, c, fmt.Errorf("%w: issuer 不匹配（%q）", ErrUnauthenticated, c.Issuer)
	}
	auds := uniqAudience(audienceOf(c.Audience))
	found := false
	for _, a := range auds {
		if a == p.ClientID {
			found = true
			break
		}
	}
	if !found {
		return nil, nil, c, fmt.Errorf("%w: audience 不含 client_id", ErrUnauthenticated)
	}
	if c.Subject == "" {
		return nil, nil, c, fmt.Errorf("%w: 缺少 sub", ErrUnauthenticated)
	}
	out := &Claims{
		Subject:   c.Subject,
		Email:     c.Email,
		Groups:    groupsFrom(raw, "groups"),
		Issuer:    c.Issuer,
		Audience:  auds,
		ExpiresAt: time.Unix(c.Exp, 0),
		NotBefore: time.Unix(c.Nbf, 0),
		IssuedAt:  time.Unix(c.Iat, 0),
	}
	return out, raw, c, nil
}

// VerifyJWT 校验 Bearer JWT（服务间调用）。
func (p *Provider) VerifyJWT(ctx context.Context, token string) (*Claims, rawClaims, error) {
	claims, raw, _, err := p.verifyToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	return claims, raw, nil
}

// LoginURL 生成授权码 + PKCE 的跳转地址。
func (p *Provider) LoginURL(ctx context.Context, state, nonce, codeChallenge string) (string, error) {
	doc, err := p.Discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", p.RedirectURL)
	q.Set("scope", strings.Join(p.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return doc.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Exchange 用授权码换 token，并校验 ID token（签名 + 标准 claims + nonce）。
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier, expectNonce string) (*Claims, rawClaims, error) {
	doc, err := p.Discover(ctx)
	if err != nil {
		return nil, nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.RedirectURL)
	form.Set("code_verifier", codeVerifier)
	if p.ClientAuth == "post" {
		form.Set("client_id", p.ClientID)
		if p.ClientSecret != "" {
			form.Set("client_secret", p.ClientSecret)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("auth: token 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.ClientAuth != "post" { // basic（默认）
		req.SetBasicAuth(p.ClientID, p.ClientSecret)
	}

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: token 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%w: token 端点返回 %d: %s",
			ErrUnauthenticated, resp.StatusCode, truncate(string(body), 200))
	}
	var tok struct {
		IDToken   string `json:"id_token"`
		Error     string `json:"error,omitempty"`
		ErrorDesc string `json:"error_description,omitempty"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, nil, fmt.Errorf("auth: token 响应不是 JSON: %w", err)
	}
	if tok.Error != "" {
		return nil, nil, fmt.Errorf("%w: token 端点错误 %s %s", ErrUnauthenticated, tok.Error, tok.ErrorDesc)
	}
	if tok.IDToken == "" {
		return nil, nil, fmt.Errorf("%w: token 响应缺少 id_token", ErrUnauthenticated)
	}
	claims, raw, idClaims, err := p.verifyToken(ctx, tok.IDToken)
	if err != nil {
		return nil, nil, err
	}
	// nonce 必须与授权请求里的一致（防重放）。
	if expectNonce != "" && idClaims.Nonce != expectNonce {
		return nil, nil, fmt.Errorf("%w: nonce 不匹配", ErrUnauthenticated)
	}
	return claims, raw, nil
}

func mustHeader(token string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64RawURL.DecodeString(parts[0])
	if err != nil {
		return nil
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
