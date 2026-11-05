// Package auth 实现 07 §2 的鉴权：OIDC authorization code + PKCE（浏览器）、
// JWKS 校验的 Bearer JWT（服务间）、签名会话 Cookie 与 CSRF 双提交、以及 RBAC 角色映射。
//
// 约束（D-51）：
//   - TestPlane 是独立 OIDC client，与 Genet 共用企业身份源但**不共享会话**、不把 Genet 当 IdP；
//   - 浏览器与机器调用最终落到**同一套 claims → 同一套 RBAC**，不设后门；
//   - 不引第三方库：JWT 验签用 crypto/rsa + encoding/base64，JWKS 用 encoding/json + big.Int。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 角色（07 §2.2）。数字用于"至少需要"比较。
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// AtLeast 报告 role 是否达到 want。
func AtLeast(role, want string) bool { return roleRank(role) >= roleRank(want) }

// RoleMapper 把 claims 里的 groups 映射成角色（TESTPLANE_ROLE_CLAIM / TESTPLANE_ROLE_MAP）。
type RoleMapper struct {
	Claim string
	Map   map[string]string
}

// RoleFor 选角色：按 groups 里**最高**的那个映射；都没命中 → viewer。
func (m RoleMapper) RoleFor(groups []string) string {
	best := ""
	for _, g := range groups {
		r, ok := m.Map[strings.TrimSpace(g)]
		if !ok {
			continue
		}
		if best == "" || roleRank(r) > roleRank(best) {
			best = r
		}
	}
	if best == "" {
		return RoleViewer
	}
	return best
}

// ErrUnauthenticated 表示凭据缺失/无效/过期（映射 401 UNAUTHENTICATED）。
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Claims 是平台真正关心的 OIDC claims（ID token 与 Bearer JWT 共用）。
type Claims struct {
	Subject   string   `json:"sub"`
	Email     string   `json:"email"`
	Groups    []string `json:"groups"`
	Issuer    string   `json:"iss"`
	Audience  []string `json:"-"`
	ExpiresAt time.Time
	NotBefore time.Time
	IssuedAt  time.Time
}

// Identity 是校验通过后的请求身份（middleware 注入 ctx 用）。
type Identity struct {
	User   string
	Email  string
	Role   string
	Groups []string
	// Via 是凭据来源：cookie（浏览器）或 bearer（服务间）。CSRF 只对 cookie 生效。
	Via string
}

// Session 是 `tp_session` Cookie 的载荷（签名后 base64url）。
type Session struct {
	Sub    string   `json:"sub"`
	Email  string   `json:"email"`
	Role   string   `json:"role"`
	Groups []string `json:"groups,omitempty"`
	CSRF   string   `json:"csrf"`
	Exp    int64    `json:"exp"`
}

// Valid 报告会话是否未过期。
func (s Session) Valid(now time.Time) bool { return s.Exp > now.Unix() }

func uniqAudience(aud []string) []string {
	out := make([]string, 0, len(aud))
	seen := map[string]bool{}
	for _, a := range aud {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

var _ = fmt.Sprintf
