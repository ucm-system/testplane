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
	// AuthMode 是鉴权模式（TESTPLANE_AUTH_MODE，MVP 只有 dev）。
	AuthMode string
	// APIPrefix 是业务端点前缀，固定 /api/v1（07 §1）。
	APIPrefix string
}

// Load 从环境变量装载配置并校验。任何非法值都返回错误（调用方直接退出）。
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:             envFirst("TESTPLANE_HTTP_ADDR", "TESTPLANE_LISTEN", ":8080"),
		DatabaseURL:          envFirst("TESTPLANE_DATABASE_URL", "TESTPLANE_DB_DSN", ""),
		StorageRoot:          env("TESTPLANE_STORAGE_ROOT", "/storage-root"),
		Role:                 Role(strings.ToLower(env("TESTPLANE_ROLE", string(RoleAll)))),
		LogLevel:             strings.ToLower(env("TESTPLANE_LOG_LEVEL", "info")),
		TestersDir:           env("TESTPLANE_TESTERS_DIR", "./testers"),
		Kubeconfig:           env("TESTPLANE_KUBECONFIG", ""),
		DBMaxConns:           20,
		RunMaxTasks:          200,
		TaskTimeoutDefault:   4 * time.Hour,
		TaskTimeoutMax:       24 * time.Hour,
		ResultMaxBytes:       100 << 20,
		SSEHeartbeat:         15 * time.Second,
		SSEReplayWindow:      24 * time.Hour,
		AuthMode:             strings.ToLower(env("TESTPLANE_AUTH_MODE", "dev")),
		APIPrefix:            "/api/v1",
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
	switch c.AuthMode {
	case "dev", "oidc":
	default:
		return nil, fmt.Errorf("config: TESTPLANE_AUTH_MODE=%q 非法，允许 dev|oidc", c.AuthMode)
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
