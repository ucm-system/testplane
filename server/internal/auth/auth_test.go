package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionCodecRoundTripAndTamper(t *testing.T) {
	c := SessionCodec{Secret: []byte("0123456789abcdef0123456789abcdef")}
	s := Session{Sub: "alice", Role: RoleOperator, Groups: []string{"tp-ops"}, Exp: time.Now().Add(time.Hour).Unix()}
	tok, err := c.Seal(s)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var got Session
	if err := c.Open(tok, &got); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Sub != s.Sub || got.Role != s.Role || len(got.Groups) != 1 {
		t.Fatalf("往返不一致: %+v", got)
	}
	// 篡改 payload → 签名不符
	parts := strings.SplitN(tok, ".", 2)
	tampered := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"evil"}`)) + "." + parts[1]
	if err := c.Open(tampered, &got); err == nil {
		t.Fatal("篡改后的会话必须被拒")
	}
	// 换密钥 → 拒
	other := SessionCodec{Secret: []byte("ffffffffffffffffffffffffffffffff")}
	if err := other.Open(tok, &got); err == nil {
		t.Fatal("跨密钥会话必须被拒")
	}
}

func TestCSRFTokenStableAndBound(t *testing.T) {
	c := SessionCodec{Secret: []byte("0123456789abcdef0123456789abcdef")}
	s1 := Session{Sub: "alice", Exp: 1000}
	s2 := Session{Sub: "bob", Exp: 1000}
	if c.CSRFToken(s1) == "" || c.CSRFToken(s1) != c.CSRFToken(s1) {
		t.Fatal("CSRF token 必须稳定非空")
	}
	if c.CSRFToken(s1) == c.CSRFToken(s2) {
		t.Fatal("不同用户必须得到不同 CSRF token")
	}
}

func TestRoleMapper(t *testing.T) {
	m := RoleMapper{Claim: "groups", Map: map[string]string{
		"tp-admins": RoleAdmin, "tp-ops": RoleOperator,
	}}
	cases := []struct {
		groups []string
		want   string
	}{
		{[]string{"tp-ops"}, RoleOperator},
		{[]string{"tp-admins"}, RoleAdmin},
		{[]string{"tp-ops", "tp-admins"}, RoleAdmin}, // 取最高
		{[]string{"other"}, RoleViewer},              // 未命中 → viewer
		{nil, RoleViewer},
	}
	for _, c := range cases {
		if got := m.RoleFor(c.groups); got != c.want {
			t.Fatalf("RoleFor(%v) = %q, 期望 %q", c.groups, got, c.want)
		}
	}
	if !AtLeast(RoleAdmin, RoleOperator) || AtLeast(RoleViewer, RoleOperator) {
		t.Fatal("AtLeast 实现错误")
	}
}

func TestSafeReturn(t *testing.T) {
	cases := map[string]string{
		"/runs/r-1":        "/runs/r-1",
		"":                 "/",
		"http://evil.com":  "/",
		"//evil.com/x":     "/",
		"javascript:alert": "/",
		"runs":             "/",
	}
	for in, want := range cases {
		if got := safeReturn(in); got != want {
			t.Fatalf("safeReturn(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// fakeJWTIdP 是最小 JWKS 提供方（只测 VerifyJWT，不走浏览器流）。
func fakeJWTIdP(t *testing.T, clientID string) (*httptest.Server, *rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 key: %v", err)
	}
	kid := "kid-1"
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize",
			"token_endpoint": base + "/token", "jwks_uri": base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv, key, kid
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any, alg string) string {
	t.Helper()
	hb, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": kid})
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	if alg != "RS256" {
		return signing + "." + base64.RawURLEncoding.EncodeToString([]byte("bogus"))
	}
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("签名: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyJWT(t *testing.T) {
	srv, key, kid := fakeJWTIdP(t, "testplane")
	p := &Provider{Issuer: srv.URL, ClientID: "testplane"}

	good := signTestJWT(t, key, kid, map[string]any{
		"iss": srv.URL, "sub": "alice", "aud": "testplane",
		"groups": []string{"tp-ops"}, "exp": time.Now().Add(5 * time.Minute).Unix(),
	}, "RS256")
	claims, raw, err := p.VerifyJWT(context.Background(), good)
	if err != nil {
		t.Fatalf("合法 token 被拒: %v", err)
	}
	if claims.Subject != "alice" || len(claims.Groups) != 1 {
		t.Fatalf("claims 异常: %+v", claims)
	}
	if groupsFrom(raw, "groups")[0] != "tp-ops" {
		t.Fatalf("groups 提取异常: %v", raw["groups"])
	}

	bad := []struct {
		name   string
		token  string
		reason string
	}{
		{"过期", signTestJWT(t, key, kid, map[string]any{
			"iss": srv.URL, "sub": "a", "aud": "testplane", "exp": time.Now().Add(-5 * time.Minute).Unix(),
		}, "RS256"), "exp"},
		{"错误 aud", signTestJWT(t, key, kid, map[string]any{
			"iss": srv.URL, "sub": "a", "aud": "other", "exp": time.Now().Add(5 * time.Minute).Unix(),
		}, "RS256"), "aud"},
		{"错误 iss", signTestJWT(t, key, kid, map[string]any{
			"iss": "https://evil", "sub": "a", "aud": "testplane", "exp": time.Now().Add(5 * time.Minute).Unix(),
		}, "RS256"), "iss"},
		{"alg 混淆", signTestJWT(t, key, kid, map[string]any{
			"iss": srv.URL, "sub": "a", "aud": "testplane", "exp": time.Now().Add(5 * time.Minute).Unix(),
		}, "HS256"), "alg"},
		{"缺少 exp", signTestJWT(t, key, kid, map[string]any{
			"iss": srv.URL, "sub": "a", "aud": "testplane",
		}, "RS256"), "exp"},
	}
	for _, c := range bad {
		if _, _, err := p.VerifyJWT(context.Background(), c.token); err == nil {
			t.Fatalf("%s 场景必须被拒", c.name)
		}
	}
}

func TestPKCEChallenge(t *testing.T) {
	// RFC 7636 附录 B 的示例 verifier。
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := PKCEChallenge(verifier); got != want {
		t.Fatalf("PKCEChallenge = %q, 期望 %q", got, want)
	}
}
