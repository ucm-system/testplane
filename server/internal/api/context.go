// Package api 是 HTTP 边界：路由、中间件、鉴权、错误映射、SSE。
//
// 规则（05 §3.1）：api 只做鉴权、校验、读写投影；不直接调 executor、不写 SQL，
// 状态推进一律通过 store 门面。所有业务端点在 /api/v1 下（07 §1），
// /healthz 与 /readyz 不带版本。
package api

import (
	"context"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyUser
	ctxKeyRole
	ctxKeyEmail
	ctxKeyGroups
	ctxKeyVia
)

// WithIdentityFull 注入完整身份（oidc 模式用：含 email/groups/凭据来源）。
func WithIdentityFull(ctx context.Context, user, role, email string, groups []string, via string) context.Context {
	ctx = WithIdentity(ctx, user, role)
	ctx = context.WithValue(ctx, ctxKeyEmail, email)
	ctx = context.WithValue(ctx, ctxKeyGroups, groups)
	return context.WithValue(ctx, ctxKeyVia, via)
}

// Email 取当前用户邮箱（dev 模式为空）。
func Email(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyEmail).(string); ok {
		return v
	}
	return ""
}

// Groups 取 claims 里的 groups（RBAC 的原始输入，展示用）。
func Groups(ctx context.Context) []string {
	if v, ok := ctx.Value(ctxKeyGroups).([]string); ok {
		return v
	}
	return nil
}

// Via 是凭据来源：cookie | bearer | dev。
func Via(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyVia).(string); ok {
		return v
	}
	return "dev"
}

// WithIdentity 把身份写进 ctx（中间件用）。
func WithIdentity(ctx context.Context, user, role string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyUser, user)
	return context.WithValue(ctx, ctxKeyRole, role)
}

// User 取当前请求的用户名（dev 模式缺省 dev）。
func User(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyUser).(string); ok && v != "" {
		return v
	}
	return "dev"
}

// Role 取当前请求的角色（dev 模式固定 admin）。
func Role(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRole).(string); ok && v != "" {
		return v
	}
	return "admin"
}

// WithRequestID 把 request id 写进 ctx。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID 取当前请求的 id（响应头 X-Request-Id 与错误体 trace_id 都用它）。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// Actor 构造运维/审计用的 actor 字符串：user:<id>。
func Actor(ctx context.Context) string { return "user:" + User(ctx) }
