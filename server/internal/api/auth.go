package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ucm-system/testplane/server/internal/auth"
	"github.com/ucm-system/testplane/server/internal/domain"
)

// buildAuthenticator 从配置构造 auth.Authenticator（dev 模式也走同一个实现，
// 只是 FromRequest 退化成 `Bearer <subject>` 即 admin）。
func (s *Server) buildAuthenticator() *auth.Authenticator {
	a, err := auth.New(auth.Config{
		Mode:          s.cfg.AuthMode,
		Issuer:        s.cfg.OIDCIssuer,
		ClientID:      s.cfg.OIDCClientID,
		ClientSecret:  s.cfg.OIDCClientSecret,
		ClientAuth:    s.cfg.OIDCClientAuth,
		RedirectURL:   s.cfg.OIDCRedirectURL,
		Scopes:        s.cfg.OIDCScopes,
		RoleClaim:     s.cfg.OIDCRoleClaim,
		RoleMap:       s.cfg.RoleMap,
		SessionSecret: s.cfg.SessionSecret,
		CookieName:    s.cfg.SessionCookie,
		TTL:           s.cfg.SessionTTL,
		Secure:        s.cfg.SessionSecure,
		Now:           s.now,
	})
	if err != nil {
		// 配置层已挡住 oidc 缺项；走到这里说明装配有误，启动即失败比运行期 500 好。
		panic("api: 构造 Authenticator 失败: " + err.Error())
	}
	// 红线（07 §2.1 / C-10）：dev 鉴权用在非本地环境必须留痕。
	if a.Mode() == "dev" && s.cfg.Env != "dev" {
		s.logger.Warn("TESTPLANE_AUTH_MODE=dev 用在非 dev 环境：任何人都能以任意身份操作，请切到 oidc",
			"env", s.cfg.Env)
	}
	return a
}

// middlewareAuth 解析身份并注入 ctx；随后做 CSRF 校验（仅 Cookie 会话）。
// 免鉴权路径（/healthz、/readyz、/metrics、/auth/login|callback）不在本中间件组内。
func (s *Server) middlewareAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.authn.FromRequest(r)
		if err != nil {
			s.failAuth(w, r, err)
			return
		}
		// CSRF：写操作且凭据是 Cookie 时必须带 X-CSRF-Token（Bearer 天然免疫）。
		if isUnsafeMethod(r.Method) && !s.authn.CheckCSRF(r, id) {
			// 拒绝也要留痕（PR #33 #6）：身份有效，缺的是防伪令牌。
			s.auditRejected(r.Context(), "user:"+id.User, id.Role, actionCSRFRejected, objectHTTPRequest, r.URL.Path,
				map[string]any{
					"result": "rejected", "code": domain.CodeForbidden,
					"method": r.Method, "path": r.URL.Path,
					"role": id.Role, "reason": "csrf",
				})
			fail(w, r, http.StatusForbidden, domain.CodeForbidden,
				"缺少或错误的 X-CSRF-Token（写操作需双提交；token 来自 GET /me）", nil, false)
			return
		}
		ctx := WithIdentityFull(r.Context(), id.User, id.Role, id.Email, id.Groups, id.Via)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// 接入层拒绝的 action（与 03 §4.6.1 的动作枚举对齐）：
//   - rbac_rejected：角色不足（403，进不到 handler）
//   - csrf_rejected：Cookie 写操作缺/错 X-CSRF-Token（403）
//   - auth_rejected：凭据缺失/无效（401，actor=anonymous）
//
// object_type 统一用 http_request，object_id 记请求路径，detail 里带 method/path/role/reason。
const (
	actionRBACRejected = "rbac_rejected"
	actionCSRFRejected = "csrf_rejected"
	actionAuthRejected = "auth_rejected"
	objectHTTPRequest  = "http_request"
)

// requireRole 是 RBAC 门：角色不足返回 403 FORBIDDEN（07 §2.3）。
// 拒绝会写一条 result=rejected 审计（review #6：中间件拦截进不到 handler，也必须留痕）。
func (s *Server) requireRole(min string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := Role(r.Context())
			if !auth.AtLeast(role, min) {
				s.auditRejected(r.Context(), Actor(r.Context()), role,
					actionRBACRejected, objectHTTPRequest, r.URL.Path,
					map[string]any{
						"result": "rejected", "code": domain.CodeForbidden,
						"method": r.Method, "path": r.URL.Path,
						"role": role, "required": min, "reason": "rbac",
					})
				fail(w, r, http.StatusForbidden, domain.CodeForbidden,
					"需要 "+min+" 及以上角色", map[string]any{"required": min, "role": role}, false)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) failAuth(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) {
		// 401 也留痕（actor=anonymous）：合规要能查"谁被拒了"。
		s.auditRejected(r.Context(), "anonymous", "anonymous", actionAuthRejected, objectHTTPRequest, r.URL.Path,
			map[string]any{
				"result": "rejected", "code": domain.CodeUnauthenticated,
				"method": r.Method, "path": r.URL.Path, "reason": "unauthenticated",
			})
		fail(w, r, http.StatusUnauthorized, domain.CodeUnauthenticated, err.Error(), nil, false)
		return
	}
	s.failErr(w, r, err)
}

func isUnsafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// ---- /auth/* 端点（匿名，注册在根路由；07 §2.4）------------------------------

// handleAuthLogin 是 GET /auth/login?return=<path>：302 到 IdP（PKCE）。
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.authn.Mode() != "oidc" {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound,
			"当前是 dev 鉴权模式，没有登录流程", nil, false)
		return
	}
	target, cookie, err := s.authn.Begin(r.Context(), r.URL.Query().Get("return"))
	if err != nil {
		fail(w, r, http.StatusBadGateway, domain.CodeInternalError, "无法开始登录: "+err.Error(), nil, true)
		return
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, target, http.StatusFound)
}

// handleAuthCallback 是 GET /auth/callback?code=&state=：换会话、Set-Cookie、302 回前端。
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.authn.Mode() != "oidc" {
		fail(w, r, http.StatusNotFound, domain.CodeNotFound,
			"当前是 dev 鉴权模式，没有登录流程", nil, false)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(w, r, http.StatusUnauthorized, domain.CodeUnauthenticated,
			"IdP 返回错误: "+e+" "+q.Get("error_description"), nil, false)
		return
	}
	cookie, returnTo, err := s.authn.Complete(r.Context(), r, q.Get("code"), q.Get("state"))
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			fail(w, r, http.StatusUnauthorized, domain.CodeUnauthenticated, err.Error(), nil, false)
			return
		}
		s.failErr(w, r, err)
		return
	}
	http.SetCookie(w, s.authn.TxClearCookie())
	http.SetCookie(w, cookie)
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// handleAuthLogout 是 POST /auth/logout：清会话 Cookie。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.authn.LogoutCookie())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// meDTO 是 GET /me 的响应（07 §2.4）：oidc 模式下带 csrf_token 与 groups。
type meDTO struct {
	User      string   `json:"user"`
	Role      string   `json:"role"`
	CSRFToken string   `json:"csrf_token,omitempty"`
	Groups    []string `json:"groups,omitempty"`
	Email     string   `json:"email,omitempty"`
}

func (s *Server) currentIdentity(r *http.Request) meDTO {
	dto := meDTO{User: User(r.Context()), Role: Role(r.Context()), Email: Email(r.Context())}
	if groups := Groups(r.Context()); len(groups) > 0 {
		dto.Groups = groups
	}
	if ct, ok := s.authn.CSRFFor(r); ok {
		dto.CSRFToken = ct
	}
	return dto
}

// csrfHeaderMissing 给测试与诊断用：判断请求是否缺 CSRF 头。
func csrfHeaderMissing(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("X-CSRF-Token")) == ""
}

// RBAC 角色常量别名（api 层不直接 import 常量名太长）。
const (
	authRoleViewer   = auth.RoleViewer
	authRoleOperator = auth.RoleOperator
	authRoleAdmin    = auth.RoleAdmin
)
