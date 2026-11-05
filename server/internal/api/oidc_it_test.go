package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// fakeIdP 是最小 OIDC Provider：discovery + JWKS + authorize（直接 302 回带 code）+ token。
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	client string

	mu    sync.Mutex
	code  string
	nonce string // 授权时记下的 nonce，token 时用来产出 id_token
	sub   string
	email string
	groups []string
	expDelta time.Duration
	aud   string
	genNonce string // 强制写进 id_token 的 nonce（测 nonce 不匹配）
}

func newFakeIdP(t *testing.T, clientID string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA key: %v", err)
	}
	idp := &fakeIdP{t: t, key: key, kid: "test-kid-1", client: clientID,
		code: "test-code-1", sub: "alice", email: "alice@example.com",
		groups: []string{"tp-ops"}, expDelta: 5 * time.Minute, aud: clientID}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := idp.base()
		writeJSONRaw(w, map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"jwks_uri":               base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
		writeJSONRaw(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": idp.kid, "use": "sig", "alg": "RS256", "n": n, "e": e,
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		idp.mu.Lock()
		idp.nonce = q.Get("nonce")
		idp.mu.Unlock()
		target := q.Get("redirect_uri") + "?code=" + url.QueryEscape(idp.code) +
			"&state=" + url.QueryEscape(q.Get("state"))
		http.Redirect(w, r, target, http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.FormValue("code"); got != idp.code {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		// 基本说明：允许 basic / post 两种 client 认证。
		if u, _, ok := r.BasicAuth(); ok && u != "" && u != idp.client {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		token := idp.signToken(map[string]any{
			"iss":    idp.base(),
			"sub":    idp.sub,
			"aud":    idp.aud,
			"email":  idp.email,
			"groups": idp.groups,
			"nonce":  idp.tokenNonce(),
			"exp":    time.Now().Add(idp.expDelta).Unix(),
			"iat":    time.Now().Unix(),
		})
		writeJSONRaw(w, map[string]any{"id_token": token, "access_token": "at", "token_type": "Bearer"})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (f *fakeIdP) base() string { return f.srv.URL }

func (f *fakeIdP) tokenNonce() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.genNonce != "" {
		return f.genNonce
	}
	return f.nonce
}

// signToken 用 RS256 手写签名（与实现路径无关，避免复用被测代码）。
func (f *fakeIdP) signToken(claims map[string]any) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": f.kid}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatalf("签名失败: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSONRaw(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

// oidcTestServer 起一个 oidc 模式的 api.Server（假 IdP + 真 PG）。
func oidcTestServer(t *testing.T, idp *fakeIdP) (*Server, *store.Store, func()) {
	t.Helper()
	dsn, err := dbtest.URL("api")
	if err != nil || dsn == "" {
		if err != nil {
			t.Fatalf("准备测试库: %v", err)
		}
		t.Skip("未设置 TESTPLANE_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	if err := st.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := &config.Config{
		APIPrefix: "/api/v1", StorageRoot: t.TempDir(),
		RunMaxTasks: 200, TaskTimeoutDefault: time.Hour, TaskTimeoutMax: 24 * time.Hour,
		MaxGlobalConcurrency: 4, DeployTimeout: 72 * time.Hour,
		AuthMode:        "oidc",
		Env:             "dev",
		OIDCIssuer:      idp.base(),
		OIDCClientID:    idp.client,
		OIDCClientSecret: "secret",
		OIDCClientAuth:  "basic",
		OIDCRedirectURL: "http://testplane.local/auth/callback",
		OIDCScopes:      []string{"openid", "email", "groups"},
		OIDCRoleClaim:   "groups",
		RoleMap:         map[string]string{"tp-admins": "admin", "tp-ops": "operator"},
		SessionSecret:   []byte("0123456789abcdef0123456789abcdef"),
		SessionCookie:   "tp_session",
		SessionTTL:      time.Hour,
		SessionSecure:   false, // httptest 用 http
	}
	resolver := &fakeResolver{specs: map[string]domain.TesterSpec{
		"probe-again@1.0.0": {
			Name: "probe-again", Version: "1.0.0", Role: domain.RoleTest,
			OnFailure: domain.OnFailureContinue, Entry: "/tester/run.sh", Image: "probe:1",
		},
	}}
	srv := New(Deps{Config: cfg, Store: st, Machines: store.NewMachines(st, nil), Resolver: resolver})
	return srv, st, func() { st.Close() }
}

const validSubmitYAML = `
run:
  name: oidc-demo
  precheck: "off"
  service:
    url: http://127.0.0.1:8000/v1
  tests:
    - name: t1
      tester: probe-again
      version: "1.0.0"
      execution:
        profile: k8s-npu
`

// browserLogin 走完"授权码 → 回调 → 会话 Cookie"，返回会话 Cookie 与 /me 的 csrf_token。
func browserLogin(t *testing.T, srv *Server, idp *fakeIdP) (*http.Cookie, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?return=/runs", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, body=%s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("解析 Location: %v", err)
	}
	if !strings.Contains(loc.Path, "/authorize") {
		t.Fatalf("应跳转到 IdP authorize，得到 %s", loc)
	}
	if loc.Query().Get("code_challenge") == "" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("缺少 PKCE 参数: %s", loc.RawQuery)
	}
	// 真的访问一次 IdP 的 authorize（不跟随 302），让假 IdP 记下 nonce。
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	idpResp, err := noFollow.Get(loc.String())
	if err != nil {
		t.Fatalf("访问 authorize: %v", err)
	}
	_ = idpResp.Body.Close()
	state := loc.Query().Get("state")
	var txCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tp_auth_tx" {
			txCookie = c
		}
	}
	if txCookie == nil {
		t.Fatal("登录未下发 tp_auth_tx 中间态 Cookie")
	}

	// 回调（带中间态 Cookie）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/auth/callback?code="+url.QueryEscape(idp.code)+"&state="+url.QueryEscape(state), nil)
	req.AddCookie(txCookie)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/runs" {
		t.Fatalf("callback 应回跳 /runs，得到 %q", got)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tp_session" {
			session = c
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("回调未写会话 Cookie")
	}
	if !session.HttpOnly {
		t.Fatal("会话 Cookie 必须 HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("会话 Cookie SameSite = %v，期望 Lax", session.SameSite)
	}

	// /me 取 csrf_token
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(session)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/me status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var me struct {
		User      string `json:"user"`
		Role      string `json:"role"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("解析 /me: %v", err)
	}
	if me.User != "alice" || me.CSRFToken == "" {
		t.Fatalf("/me 异常: %+v", me)
	}
	return session, me.CSRFToken
}

// TestOIDC_LoginFlow 覆盖登录成功、角色映射、/me 下发 CSRF。
func TestOIDC_LoginFlow(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	session, csrf := browserLogin(t, srv, idp)
	if csrf == "" {
		t.Fatal("缺少 csrf_token")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(session)
	srv.Handler().ServeHTTP(rec, req)
	var me struct {
		User   string   `json:"user"`
		Role   string   `json:"role"`
		Groups []string `json:"groups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Role != "operator" {
		t.Fatalf("角色 = %q, 期望 operator（groups=tp-ops）", me.Role)
	}
	if len(me.Groups) != 1 || me.Groups[0] != "tp-ops" {
		t.Fatalf("groups 异常: %+v", me.Groups)
	}
}

// TestOIDC_StateMismatch → 401。
func TestOIDC_StateMismatch(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	var tx *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tp_auth_tx" {
			tx = c
		}
	}
	if tx == nil {
		t.Fatal("缺少中间态 Cookie")
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+idp.code+"&state=wrong-state", nil)
	req.AddCookie(tx)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("state 不匹配 status = %d, 期望 401, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("错误码应为 UNAUTHENTICATED: %s", rec.Body.String())
	}
}

// TestOIDC_NonceMismatch → 401。id_token 里的 nonce 与授权请求不一致。
func TestOIDC_NonceMismatch(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	idp.genNonce = "totally-different-nonce"
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	var tx *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tp_auth_tx" {
			tx = c
		}
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/auth/callback?code="+idp.code+"&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	req.AddCookie(tx)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nonce 不匹配 status = %d, 期望 401", rec.Code)
	}
}

// TestOIDC_BearerExpired 过期 token → 401。
func TestOIDC_BearerExpired(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	token := idp.signToken(map[string]any{
		"iss": idp.base(), "sub": "bob", "aud": idp.client,
		"groups": []string{"tp-ops"},
		"exp":    time.Now().Add(-10 * time.Minute).Unix(),
		"iat":    time.Now().Add(-20 * time.Minute).Unix(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("过期 token status = %d, 期望 401, body=%s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_BearerWrongAudience → 401。
func TestOIDC_BearerWrongAudience(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	token := idp.signToken(map[string]any{
		"iss": idp.base(), "sub": "bob", "aud": "some-other-client",
		"groups": []string{"tp-admins"},
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 aud status = %d, 期望 401", rec.Code)
	}
}

// TestOIDC_CSRFRequired 写操作缺 X-CSRF-Token → 403；带上则通过。
func TestOIDC_CSRFRequired(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	session, csrf := browserLogin(t, srv, idp)

	// 缺 CSRF → 403
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(validSubmitYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.AddCookie(session)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF status = %d, 期望 403, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "FORBIDDEN") {
		t.Fatalf("错误码应为 FORBIDDEN: %s", rec.Body.String())
	}

	// 带正确 CSRF → 202
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(validSubmitYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(session)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("带 CSRF status = %d, 期望 202, body=%s", rec.Code, rec.Body.String())
	}
}

// TestOIDC_RBACViewerDenied viewer 调写接口 → 403（用 Bearer 免 CSRF，单独验证 RBAC）。
func TestOIDC_RBACViewerDenied(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	token := idp.signToken(map[string]any{
		"iss": idp.base(), "sub": "carol", "aud": idp.client,
		"groups": []string{"some-other-group"}, // 未命中映射 → viewer
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(validSubmitYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 写 status = %d, 期望 403, body=%s", rec.Code, rec.Body.String())
	}

	// 读接口允许
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer 读 status = %d, 期望 200", rec.Code)
	}
}

// TestOIDC_RBACAdminAllowed admin 通过；同一 Bearer 走同一套 RBAC。
func TestOIDC_RBACAdminAllowed(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	token := idp.signToken(map[string]any{
		"iss": idp.base(), "sub": "root", "aud": idp.client,
		"groups": []string{"tp-admins"},
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(validSubmitYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin 写 status = %d, 期望 202, body=%s", rec.Code, rec.Body.String())
	}

	// /me 也走同一套 claims
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	var me struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Role != "admin" {
		t.Fatalf("/me role = %q, 期望 admin", me.Role)
	}
}

// TestOIDC_MetricsStillAnonymous 切 oidc 后 /metrics 仍匿名（无会话/无令牌）。
func TestOIDC_MetricsStillAnonymous(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 在 oidc 模式必须匿名可读，得到 %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "testplane_up 1") {
		t.Fatalf("/metrics 内容异常")
	}

	// 业务端点无凭据 → 401（证明鉴权确实生效，不是全放行）。
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据访问 /api/v1/runs status = %d, 期望 401", rec.Code)
	}
}

// TestOIDC_LogoutClearsSession logout 清 Cookie；之后无凭据 401。
func TestOIDC_LogoutClearsSession(t *testing.T) {
	idp := newFakeIdP(t, "testplane")
	srv, _, cleanup := oidcTestServer(t, idp)
	defer cleanup()
	session, csrf := browserLogin(t, srv, idp)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(session)
	req.Header.Set("X-CSRF-Token", csrf) // 写操作需要 CSRF（logout 也在鉴权组内）
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tp_session" {
			cleared = c
		}
	}
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("logout 应清空会话 Cookie: %+v", cleared)
	}
}

var _ = fmt.Sprintf
