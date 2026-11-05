// Package config 是配置的唯一入口：环境变量 → 内部字段，并在启动时做范围校验
// （fail fast）。映射规则见 design/impl/control-plane.md C-11 与 AGENTS 约定：
// 文档用 env 名，代码用内部字段名，二者指向同一个值。
//
// 环境变量统一 `TESTPLANE_` 前缀。主名取 task 口径（HTTP_ADDR / DATABASE_URL /
// MAX_GLOBAL_CONCURRENCY / DEPLOY_TIMEOUT_SECONDS），同时兼容 C-11 的老名
// （LISTEN / DB_DSN / CONCURRENCY_MAX / DEPLOY_TIMEOUT），两份都认、主名优先。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Role 是进程角色（单二进制多角色，05 §1）。
type Role string

const (
	RoleAPI        Role = "api"
	RoleController Role = "controller"
	RoleCollector  Role = "collector"
	RoleAll        Role = "all"
)

// Valid 报告角色是否合法。
func (r Role) Valid() bool {
	switch r {
	case RoleAPI, RoleController, RoleCollector, RoleAll:
		return true
	}
	return false
}

// RunsAPI / RunsController / RunsCollector 把角色展开成"这个进程要不要起某个循环"。
func (r Role) RunsAPI() bool        { return r == RoleAPI || r == RoleAll }
func (r Role) RunsController() bool { return r == RoleController || r == RoleAll }
func (r Role) RunsCollector() bool  { return r == RoleCollector || r == RoleAll }

// Config 是全部运行期配置。字段名是内部名，注释给出对应 env。
type Config struct {
	// HTTPAddr 是 API 监听地址（TESTPLANE_HTTP_ADDR / TESTPLANE_LISTEN）。
	HTTPAddr string
	// DatabaseURL 是 PostgreSQL DSN（TESTPLANE_DATABASE_URL / TESTPLANE_DB_DSN）。
	DatabaseURL string
	// StorageRoot 是产物根目录（TESTPLANE_STORAGE_ROOT），对应设计里的 <storage-root>。
	StorageRoot string
	// StoragePVC 是产物 RWX PVC 名（TESTPLANE_STORAGE_PVC，默认 testplane-storage），
	// Job 渲染时挂到 /workspace/output（execution §3.1）。
	StoragePVC string
	// ClusterName 是本实例对接的集群名（TESTPLANE_CLUSTER_NAME，默认 cluster-01），
	// 写进 attempts.executor_ref.cluster，与 clusters 表的种子对齐。
	ClusterName string
	// Role 是进程角色（TESTPLANE_ROLE）。
	Role Role
	// LogLevel 是 slog 级别（TESTPLANE_LOG_LEVEL）：debug|info|warn|error。
	LogLevel string
	// TestersDir 是 Tester 注册表扫描来源（TESTPLANE_TESTERS_DIR）。
	TestersDir string
	// MaxGlobalConcurrency 是全局并发 Task 上限（TESTPLANE_MAX_GLOBAL_CONCURRENCY /
	// TESTPLANE_CONCURRENCY_MAX）。
	MaxGlobalConcurrency int
	// DeployTimeout 是 Deploy 排队 + 部署总窗口（TESTPLANE_DEPLOY_TIMEOUT_SECONDS /
	// TESTPLANE_DEPLOY_TIMEOUT，默认 72h，见 K-6）。
	DeployTimeout time.Duration
	// Kubeconfig 透传给 executor（TESTPLANE_KUBECONFIG），平台本身不读。
	Kubeconfig string
	// DeployProvider 是 Deploy 阶段实现（TESTPLANE_DEPLOY_PROVIDER）：mock（默认）| genet。
	DeployProvider string
	// GenetBaseURL 是 Genet OpenAPI 根地址（TESTPLANE_GENET_BASE_URL），形如 https://genet.dev.huawei.com。
	GenetBaseURL string
	// GenetAPIKey 是 Genet OpenAPI 的 Bearer Key（TESTPLANE_GENET_API_KEY）；
	// 生产用 Secret 注入，本文件只读环境变量名，不落明文。
	GenetAPIKey string
	// GenetCAFile 是自签 CA 的 PEM 路径（TESTPLANE_GENET_CA_FILE），空则用系统根证书。
	GenetCAFile string
	// GenetInsecure 跳过 Genet 的 TLS 校验（TESTPLANE_GENET_INSECURE，默认 false，仅开发环境）。
	GenetInsecure bool

	// ---- 以下为实现细节配置，默认值即设计值 ----

	// DBMaxConns 是连接池上限（TESTPLANE_DB_MAX_OPEN_CONNS，默认 20）。
	DBMaxConns int32
	// RunMaxTasks 是单 Run Task 上限（TESTPLANE_RUN_MAX_TASKS，默认 200）。
	RunMaxTasks int
	// TaskTimeoutDefault 是默认 Task 超时（TESTPLANE_TASK_TIMEOUT，默认 4h）。
	TaskTimeoutDefault time.Duration
	// TaskTimeoutMax 是 Task 超时上限（TESTPLANE_TASK_TIMEOUT_MAX，默认 24h）。
	TaskTimeoutMax time.Duration
	// ResultMaxBytes 是 result.json 的技术上限（TESTPLANE_RESULT_MAX_BYTES，默认 100MiB）。
	ResultMaxBytes int64
	// SSEHeartbeat 是 SSE 心跳间隔（TESTPLANE_SSE_HEARTBEAT，默认 15s）。
	SSEHeartbeat time.Duration
	// SSEReplayWindow 是 SSE 回放窗口（TESTPLANE_SSE_REPLAY_HOURS，默认 24h）。
	SSEReplayWindow time.Duration
	// AuthMode 是鉴权模式（TESTPLANE_AUTH_MODE）：dev | oidc（07 §2）。
	AuthMode string
	// Env 是部署环境（TESTPLANE_ENV，默认 dev）：用于"dev 鉴权用在非本地就打警告"。
	Env string

	// ---- OIDC（07 §2.1、D-51：独立 client，不把 Genet 当 IdP） ----
	OIDCIssuer         string   // TESTPLANE_OIDC_ISSUER
	OIDCClientID       string   // TESTPLANE_OIDC_CLIENT_ID
	OIDCClientSecret   string   // TESTPLANE_OIDC_CLIENT_SECRET
	OIDCClientAuth     string   // TESTPLANE_OIDC_CLIENT_AUTH：basic | post（默认 basic）
	OIDCRedirectURL    string   // TESTPLANE_OIDC_REDIRECT_URL，如 http://127.0.0.1:8080/auth/callback
	OIDCScopes         []string // TESTPLANE_OIDC_SCOPES，默认 openid profile email groups
	OIDCRoleClaim      string   // TESTPLANE_OIDC_ROLE_CLAIM，默认 groups

	// ---- 会话与 CSRF ----
	SessionSecret []byte        // TESTPLANE_SESSION_SECRET（HMAC 密钥，≥32 字节）
	SessionCookie string        // TESTPLANE_SESSION_COOKIE，默认 tp_session
	SessionTTL    time.Duration // TESTPLANE_SESSION_TTL，默认 8h
	SessionSecure bool          // TESTPLANE_SESSION_SECURE，默认 true（纯内网 http 部署可关）

	// RoleMap 是 groups → 角色 的映射（TESTPLANE_ROLE_MAP，形如 tp-admins=admin,tp-ops=operator）。
	RoleMap map[string]string

	// APIPrefix 是业务端点前缀，固定 /api/v1（07 §1）。
	APIPrefix string
}

// Load 从环境变量装载配置并校验。任何非法值都返回错误（调用方直接退出）。
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:             envFirst("TESTPLANE_HTTP_ADDR", "TESTPLANE_LISTEN", ":8080"),
		DatabaseURL:          envFirst("TESTPLANE_DATABASE_URL", "TESTPLANE_DB_DSN", ""),
		StorageRoot:          env("TESTPLANE_STORAGE_ROOT", "/storage-root"),
		StoragePVC:           env("TESTPLANE_STORAGE_PVC", "testplane-storage"),
		ClusterName:          env("TESTPLANE_CLUSTER_NAME", "cluster-01"),
		Role:                 Role(strings.ToLower(env("TESTPLANE_ROLE", string(RoleAll)))),
		LogLevel:             strings.ToLower(env("TESTPLANE_LOG_LEVEL", "info")),
		TestersDir:           env("TESTPLANE_TESTERS_DIR", "./testers"),
		Kubeconfig:           env("TESTPLANE_KUBECONFIG", ""),
		DeployProvider:       strings.ToLower(env("TESTPLANE_DEPLOY_PROVIDER", "mock")),
		GenetBaseURL:         strings.TrimRight(env("TESTPLANE_GENET_BASE_URL", ""), "/"),
		GenetAPIKey:          env("TESTPLANE_GENET_API_KEY", ""),
		GenetCAFile:          env("TESTPLANE_GENET_CA_FILE", ""),
		GenetInsecure:        envBool("TESTPLANE_GENET_INSECURE", false),
		DBMaxConns:           20,
		RunMaxTasks:          200,
		TaskTimeoutDefault:   4 * time.Hour,
		TaskTimeoutMax:       24 * time.Hour,
		ResultMaxBytes:       100 << 20,
		SSEHeartbeat:         15 * time.Second,
		SSEReplayWindow:      24 * time.Hour,
		AuthMode:             strings.ToLower(env("TESTPLANE_AUTH_MODE", "dev")),
		Env:                  strings.ToLower(env("TESTPLANE_ENV", "dev")),
		OIDCIssuer:           strings.TrimRight(env("TESTPLANE_OIDC_ISSUER", ""), "/"),
		OIDCClientID:         env("TESTPLANE_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:     env("TESTPLANE_OIDC_CLIENT_SECRET", ""),
		OIDCClientAuth:       strings.ToLower(env("TESTPLANE_OIDC_CLIENT_AUTH", "basic")),
		OIDCRedirectURL:      env("TESTPLANE_OIDC_REDIRECT_URL", ""),
		OIDCScopes:           splitList(env("TESTPLANE_OIDC_SCOPES", "openid profile email groups")),
		OIDCRoleClaim:        env("TESTPLANE_OIDC_ROLE_CLAIM", "groups"),
		SessionSecret:        []byte(env("TESTPLANE_SESSION_SECRET", "")),
		SessionCookie:        env("TESTPLANE_SESSION_COOKIE", "tp_session"),
		SessionTTL:           8 * time.Hour, // 下面按 TESTPLANE_SESSION_TTL 覆盖
		SessionSecure:        envBool("TESTPLANE_SESSION_SECURE", true),
		RoleMap:              parseRoleMap(env("TESTPLANE_ROLE_MAP", "")),
		APIPrefix:            "/api/v1",
	}

	if raw := env("TESTPLANE_SESSION_TTL", ""); raw != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: TESTPLANE_SESSION_TTL=%q 非法（如 8h）", raw)
		}
		c.SessionTTL = d
	}

	if c.Role == "" {
		c.Role = RoleAll
	}
	if !c.Role.Valid() {
		return nil, fmt.Errorf("config: TESTPLANE_ROLE=%q 非法，允许 api|controller|collector|all", c.Role)
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("config: 缺少 TESTPLANE_DATABASE_URL")
	}
	if _, err := url.Parse(c.DatabaseURL); err != nil {
		return nil, fmt.Errorf("config: TESTPLANE_DATABASE_URL 非法: %w", err)
	}
	if c.StorageRoot == "" {
		return nil, errors.New("config: TESTPLANE_STORAGE_ROOT 不能为空")
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}

	var err error
	if c.DBMaxConns, err = envInt32("TESTPLANE_DB_MAX_OPEN_CONNS", c.DBMaxConns); err != nil {
		return nil, err
	}
	if c.MaxGlobalConcurrency, err = envInt("TESTPLANE_MAX_GLOBAL_CONCURRENCY", "TESTPLANE_CONCURRENCY_MAX", 4); err != nil {
		return nil, err
	}
	if c.RunMaxTasks, err = envInt("TESTPLANE_RUN_MAX_TASKS", "", c.RunMaxTasks); err != nil {
		return nil, err
	}
	if c.DeployTimeout, err = envDuration("TESTPLANE_DEPLOY_TIMEOUT_SECONDS", "TESTPLANE_DEPLOY_TIMEOUT", 72*time.Hour); err != nil {
		return nil, err
	}
	if c.TaskTimeoutDefault, err = envDuration("TESTPLANE_TASK_TIMEOUT", "", c.TaskTimeoutDefault); err != nil {
		return nil, err
	}
	if c.TaskTimeoutMax, err = envDuration("TESTPLANE_TASK_TIMEOUT_MAX", "", c.TaskTimeoutMax); err != nil {
		return nil, err
	}
	if c.ResultMaxBytes, err = envBytes("TESTPLANE_RESULT_MAX_BYTES", c.ResultMaxBytes); err != nil {
		return nil, err
	}
	if c.SSEHeartbeat, err = envDuration("TESTPLANE_SSE_HEARTBEAT", "", c.SSEHeartbeat); err != nil {
		return nil, err
	}
	if c.SSEReplayWindow, err = envDurationHours("TESTPLANE_SSE_REPLAY_HOURS", 24*time.Hour); err != nil {
		return nil, err
	}

	// 范围校验
	if c.MaxGlobalConcurrency < 1 || c.MaxGlobalConcurrency > 10000 {
		return nil, fmt.Errorf("config: TESTPLANE_MAX_GLOBAL_CONCURRENCY=%d 超出范围 [1,10000]", c.MaxGlobalConcurrency)
	}
	if c.RunMaxTasks < 1 || c.RunMaxTasks > 1000 {
		return nil, fmt.Errorf("config: TESTPLANE_RUN_MAX_TASKS=%d 超出范围 [1,1000]", c.RunMaxTasks)
	}
	if c.TaskTimeoutDefault <= 0 || c.TaskTimeoutDefault > c.TaskTimeoutMax {
		return nil, fmt.Errorf("config: 默认 Task 超时 %s 必须 >0 且 ≤ 上限 %s", c.TaskTimeoutDefault, c.TaskTimeoutMax)
	}
	// Profile CHECK 约束：deploy_timeout_seconds BETWEEN 3600 AND 604800。
	if c.DeployTimeout < time.Hour || c.DeployTimeout > 7*24*time.Hour {
		return nil, fmt.Errorf("config: Deploy 窗口 %s 超出范围 [1h,168h]（03 §3.2 的 CHECK 约束）", c.DeployTimeout)
	}
	if c.DBMaxConns < 1 {
		return nil, fmt.Errorf("config: TESTPLANE_DB_MAX_OPEN_CONNS=%d 必须 ≥1", c.DBMaxConns)
	}
	if c.ResultMaxBytes < 1 {
		return nil, errors.New("config: TESTPLANE_RESULT_MAX_BYTES 必须 >0")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("config: TESTPLANE_LOG_LEVEL=%q 非法，允许 debug|info|warn|error", c.LogLevel)
	}
	if c.OIDCClientAuth != "basic" && c.OIDCClientAuth != "post" {
		return nil, fmt.Errorf("config: TESTPLANE_OIDC_CLIENT_AUTH=%q 非法，允许 basic|post", c.OIDCClientAuth)
	}
	if c.AuthMode == "oidc" {
		var missing []string
		if c.OIDCIssuer == "" {
			missing = append(missing, "TESTPLANE_OIDC_ISSUER")
		}
		if c.OIDCClientID == "" {
			missing = append(missing, "TESTPLANE_OIDC_CLIENT_ID")
		}
		if c.OIDCRedirectURL == "" {
			missing = append(missing, "TESTPLANE_OIDC_REDIRECT_URL")
		}
		if len(c.SessionSecret) < 32 {
			missing = append(missing, "TESTPLANE_SESSION_SECRET(≥32 字节)")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("config: TESTPLANE_AUTH_MODE=oidc 缺少必需配置: %s", strings.Join(missing, ", "))
		}
	}

	switch c.AuthMode {
	case "dev", "oidc":
	default:
		return nil, fmt.Errorf("config: TESTPLANE_AUTH_MODE=%q 非法，允许 dev|oidc", c.AuthMode)
	}
	// Deploy 阶段：genet 需要完整接入配置；mock 不校验（kind E2E 用）。
	switch c.DeployProvider {
	case "mock":
	case "genet":
		if c.GenetBaseURL == "" {
			return nil, errors.New("config: TESTPLANE_DEPLOY_PROVIDER=genet 需要 TESTPLANE_GENET_BASE_URL")
		}
		if _, err := url.Parse(c.GenetBaseURL); err != nil {
			return nil, fmt.Errorf("config: TESTPLANE_GENET_BASE_URL=%q 非法: %w", c.GenetBaseURL, err)
		}
		if c.GenetAPIKey == "" {
			return nil, errors.New("config: TESTPLANE_DEPLOY_PROVIDER=genet 需要 TESTPLANE_GENET_API_KEY（生产用 Secret 注入）")
		}
		if c.GenetInsecure {
			// 不阻止启动（开发环境常见自签），但必须在日志里看得见。
			fmt.Fprintln(os.Stderr, "warning: TESTPLANE_GENET_INSECURE=true —— Genet TLS 校验已关闭，仅限开发环境")
		}
	default:
		return nil, fmt.Errorf("config: TESTPLANE_DEPLOY_PROVIDER=%q 非法，允许 mock|genet", c.DeployProvider)
	}
	return c, nil
}

// RedactedDSN 返回去掉密码的 DSN，用于日志（平台日志不打印凭据）。
func (c *Config) RedactedDSN() string {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u.User == nil {
		return "<unparsable-dsn>"
	}
	return u.Redacted()
}

// RunStorageDir 返回单个 Run 的产物目录（相对 StorageRoot 的相对路径 + 绝对路径两用）。
func (c *Config) RunStorageDir(runID string) string {
	return joinStorage(c.StorageRoot, "runs", runID)
}

func joinStorage(root string, parts ...string) string {
	out := strings.TrimRight(root, `/\`)
	for _, p := range parts {
		out += "/" + p
	}
	return out
}


// splitList 把逗号/空白分隔的字符串切成切片（OIDC scopes 用）。
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// parseRoleMap 解析 TESTPLANE_ROLE_MAP（`tp-admins=admin,tp-ops=operator`）。
// 非法条目直接忽略（宁可少给权限，不给错权限）。
func parseRoleMap(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		group, role, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		group = strings.TrimSpace(group)
		role = strings.ToLower(strings.TrimSpace(role))
		switch role {
		case "viewer", "operator", "admin":
			if group != "" {
				out[group] = role
			}
		}
	}
	return out
}

// ---- env 读取helper ------------------------------------------------------

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

// envFirst 依次尝试多个变量名，返回第一个非空值。
func envFirst(primary, alias, def string) string {
	if v, ok := os.LookupEnv(primary); ok && v != "" {
		return v
	}
	if alias != "" {
		if v, ok := os.LookupEnv(alias); ok && v != "" {
			return v
		}
	}
	return def
}

func envInt(primary, alias string, def int) (int, error) {
	raw := envFirst(primary, alias, "")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q 不是整数", primary, raw)
	}
	return n, nil
}

func envInt32(primary string, def int32) (int32, error) {
	raw := env(primary, "")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q 不是整数", primary, raw)
	}
	return int32(n), nil
}

// envDuration 接受 Go duration 字符串（72h / 30s）或纯秒数。
func envDuration(primary, alias string, def time.Duration) (time.Duration, error) {
	raw := envFirst(primary, alias, "")
	if raw == "" {
		return def, nil
	}
	raw = strings.TrimSpace(raw)
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q 不是合法时长", primary, raw)
	}
	return d, nil
}

func envDurationHours(primary string, def time.Duration) (time.Duration, error) {
	raw := env(primary, "")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("config: %s=%q 不是正整数小时", primary, raw)
	}
	return time.Duration(n) * time.Hour, nil
}

// envBytes 接受纯字节数或带单位（100MiB / 10GiB）。
func envBytes(primary string, def int64) (int64, error) {
	raw := strings.TrimSpace(env(primary, ""))
	if raw == "" {
		return def, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	upper := strings.ToUpper(raw)
	mult := int64(1)
	switch {
	case strings.HasSuffix(upper, "GIB"):
		mult, upper = 1<<30, strings.TrimSuffix(upper, "GIB")
	case strings.HasSuffix(upper, "MIB"):
		mult, upper = 1<<20, strings.TrimSuffix(upper, "MIB")
	case strings.HasSuffix(upper, "KIB"):
		mult, upper = 1<<10, strings.TrimSuffix(upper, "KIB")
	case strings.HasSuffix(upper, "GB"):
		mult, upper = 1e9, strings.TrimSuffix(upper, "GB")
	case strings.HasSuffix(upper, "MB"):
		mult, upper = 1e6, strings.TrimSuffix(upper, "MB")
	case strings.HasSuffix(upper, "KB"):
		mult, upper = 1e3, strings.TrimSuffix(upper, "KB")
	default:
		return 0, fmt.Errorf("config: %s=%q 不是合法字节数", primary, raw)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(upper), 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q 不是合法字节数", primary, raw)
	}
	return int64(n * float64(mult)), nil
}
// envBool 解析布尔型环境变量（1/true/yes 为真，0/false/no 为假，其余取默认）。
func envBool(name string, def bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}