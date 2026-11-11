// Package config 是配置的唯一入口：环境变量 → 内部字段，并在启动时做范围校验
// （fail fast）。映射规则见 design/impl/control-plane.md C-11 与 AGENTS 约定：
// 文档用 env 名，代码用内部字段名，二者指向同一个值。
//
// 环境变量统一 `AG_` 前缀（v2 更名，D-66；原 TESTPLANE_* 前缀废弃，一次切换不保留兼容）。
// 主名取 C-11 口径（AG_LISTEN / AG_DB_DSN / AG_CONCURRENCY_MAX / AG_DEPLOY_TIMEOUT）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
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
	// HTTPAddr 是 API 监听地址（AG_LISTEN）。
	HTTPAddr string
	// DatabaseURL 是 PostgreSQL DSN（AG_DB_DSN）。
	DatabaseURL string
	// StorageRoot 是产物根目录（AG_STORAGE_ROOT），对应设计里的 <storage-root>。
	StorageRoot string
	// StoragePVC 是产物 RWX PVC 名（AG_STORAGE_PVC，默认 argus-storage，D-41），
	// Job 渲染时挂到 /workspace/output。
	StoragePVC string
	// SharedMounts 是平台级统一共享挂载（D-63/D-65，AG_SHARED_MOUNTS JSON 数组）。
	// 默认空 = 不注入；AG_SHARED_MOUNTS 只在 shared_mounts 表为空时作首次种子（D-65）。
	SharedMounts []domain.SharedMountSpec
	// ClusterName 是本实例对接的默认集群名（AG_DEFAULT_CLUSTER，默认 cluster-01），
	// 写进 task_runners.executor_ref.cluster，与 clusters 表的种子对齐。
	ClusterName string
	// Role 是进程角色（AG_ROLE）。
	Role Role
	// LogLevel 是 slog 级别（AG_LOG_LEVEL）：debug|info|warn|error。
	LogLevel string
	// RunnersDir 是 Runner 注册表扫描来源（AG_RUNNERS_DIR）。
	//
	// 注意：C-11 的部署默认值是 /app/runners；仓库内联调默认 `./runners`（目录已随
	// Makefile/部署波次更名为 runners/）。
	RunnersDir string
	// Namespace 是 Job 与 PVC 所在 namespace（AG_EXECUTOR_NAMESPACE，默认 argus）。
	Namespace string
	// MaxGlobalConcurrency 是全局并发 runner 上限（AG_CONCURRENCY_MAX，默认 20，§12.7）。
	MaxGlobalConcurrency int
	// TaskMaxRunners 是单 Task runner 数上限（AG_TASK_MAX_RUNNERS，默认 200，
	// 超出拒绝 TASK_TOO_LARGE，§12.7）。
	TaskMaxRunners int
	// AttemptMax 是单 Task attempt 上限（AG_ATTEMPT_MAX，默认 3；重试超限
	// 返回 409 ATTEMPT_EXHAUSTED，§12.7）。
	AttemptMax int
	// RunnerTimeoutDefault 是默认 runner 超时（AG_RUNNER_TIMEOUT，默认 4h；
	// Manifest runners[].timeout_seconds 可覆盖，§6.6）。
	RunnerTimeoutDefault time.Duration
	// RunnerTimeoutMax 是 runner 超时上限（AG_RUNNER_TIMEOUT_MAX，默认 24h）。
	RunnerTimeoutMax time.Duration
	// DeployTimeout 是部署总窗口（AG_DEPLOY_TIMEOUT，默认 72h；= service-deploy
	// runner 的执行时限，排队计入，K-6）。
	DeployTimeout time.Duration
	// GenetBaseURL 是 Genet OpenAPI 接入点（AG_GENET_BASE_URL），仅注入
	// role=deploy/clean 的 runner Job（§9.1、§12.4）；平台自身不直连 Genet（D-70）。
	GenetBaseURL string
	// RetentionDays 是产物保留期（AG_RETENTION_DAYS，默认 30；v2 cleanup.policy
	// 取消，保留期只由本配置决定，D-70/D-33）。
	RetentionDays int
	// ResultMaxBytes 是 result.json 的技术上限（AG_RESULT_MAX_BYTES，默认 100MiB；
	// 仅防 OOM，K-10；html/csv 不受此限）。
	ResultMaxBytes int64
	// ArtifactMaxBytes 是单 attempt 产物配额（AG_ARTIFACT_MAX_BYTES，默认 10GiB；
	// 超出判失败 ARTIFACT_QUOTA_EXCEEDED，§12.7）。
	ArtifactMaxBytes int64
	// LogSegmentMaxBytes 是日志分段阈值（AG_LOG_SEGMENT_MAX_BYTES，默认 200MiB）。
	LogSegmentMaxBytes int64
	// LogTailLines 是默认日志尾部行数（AG_LOG_TAIL_LINES，默认 2000）。
	LogTailLines int
	// ReconcileInterval 是对账周期（AG_RECONCILE_INTERVAL，默认 30s，C-3.5）。
	ReconcileInterval time.Duration
	// Kubeconfig 透传给 executor（AG_KUBECONFIG），平台本身不读。
	Kubeconfig string
	// K8sInCluster 是集群内运行开关（AG_K8S_IN_CLUSTER，默认 true；false 时用 kubeconfig）。
	K8sInCluster bool
	// ClustersDir 是多集群 kubeconfig 目录（AG_CLUSTERS_DIR）。
	ClustersDir string
	// MetricsEnabled 是 /metrics 开关（AG_METRICS_ENABLED，默认 true）。
	MetricsEnabled bool
	// LogLevel 见上；Volcano 队列快照（C-4.8）：
	// VolcanoEnabled（AG_VOLCANO_ENABLED，默认 true）；关闭时 `/queue-status` 的
	// volcano 块降级为 detail=disabled。
	VolcanoEnabled bool
	// VolcanoNamespaces 是 List PodGroup 的 namespace 列表（AG_VOLCANO_NAMESPACES，
	// 逗号分隔）。空 / `*` = 全部 namespace。未配置时默认取 AG_EXECUTOR_NAMESPACE。
	VolcanoNamespaces []string
	// VolcanoTTL 是快照缓存 TTL（AG_VOLCANO_TTL，默认 15s）。
	VolcanoTTL time.Duration

	// ---- 以下为实现细节配置，默认值即设计值 ----

	// DBMaxConns 是连接池上限（AG_DB_MAX_OPEN_CONNS，默认 20）。
	DBMaxConns int32
	// SSEHeartbeat 是 SSE 心跳间隔（AG_SSE_HEARTBEAT，默认 15s）。
	SSEHeartbeat time.Duration
	// SSEReplayWindow 是 SSE 回放窗口（AG_SSE_REPLAY_HOURS，默认 24h）。
	SSEReplayWindow time.Duration
	// AuthMode 是鉴权模式（AG_AUTH_MODE）：dev | oidc（07 §2）。
	AuthMode string
	// Env 是部署环境（AG_ENV，默认 dev）：用于"dev 鉴权用在非本地就打警告"。
	Env string

	// ---- OIDC（07 §2.1、D-51：独立 client，不把 Genet 当 IdP） ----
	OIDCIssuer       string   // AG_OIDC_ISSUER
	OIDCClientID     string   // AG_OIDC_CLIENT_ID
	OIDCClientSecret string   // AG_OIDC_CLIENT_SECRET
	OIDCClientAuth   string   // AG_OIDC_CLIENT_AUTH：basic | post（默认 basic）
	OIDCRedirectURL  string   // AG_OIDC_REDIRECT_URL，如 http://127.0.0.1:8080/auth/callback
	OIDCScopes       []string // AG_OIDC_SCOPES，默认 openid profile email groups
	OIDCRoleClaim    string   // AG_OIDC_ROLE_CLAIM，默认 groups

	// ---- 会话与 CSRF ----
	SessionSecret []byte        // AG_SESSION_KEY（HMAC 密钥，≥32 字节）
	SessionCookie string        // AG_SESSION_COOKIE，默认 ag_session
	SessionTTL    time.Duration // AG_SESSION_TTL，默认 8h
	SessionSecure bool          // AG_SESSION_SECURE，默认 true（纯内网 http 部署可关）

	// RoleMap 是 groups → 角色 的映射（AG_ROLE_MAP，形如 ag-admins=admin,ag-ops=operator）。
	RoleMap map[string]string

	// JobGCEnabled 是 Job/裸 Pod 回收开关（AG_JOB_GC_ENABLED，默认 true）。
	JobGCEnabled bool

	// APIPrefix 是业务端点前缀，固定 /api/v1（07 §1）。
	APIPrefix string
}

// Load 从环境变量装载配置并校验。任何非法值都返回错误（调用方直接退出）。
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:             env("AG_LISTEN", ":8080"),
		DatabaseURL:          env("AG_DB_DSN", ""),
		StorageRoot:          env("AG_STORAGE_ROOT", "/storage-root"),
		StoragePVC:           env("AG_STORAGE_PVC", "argus-storage"),
		ClusterName:          env("AG_DEFAULT_CLUSTER", "cluster-01"),
		Role:                 Role(strings.ToLower(env("AG_ROLE", string(RoleAll)))),
		LogLevel:             strings.ToLower(env("AG_LOG_LEVEL", "info")),
		RunnersDir:           env("AG_RUNNERS_DIR", "./runners"),
		Namespace:            env("AG_EXECUTOR_NAMESPACE", "argus"),
		MaxGlobalConcurrency: 20,
		TaskMaxRunners:       200,
		AttemptMax:           3,
		RunnerTimeoutDefault: 4 * time.Hour,
		RunnerTimeoutMax:     24 * time.Hour,
		DeployTimeout:        72 * time.Hour,
		GenetBaseURL:         env("AG_GENET_BASE_URL", ""),
		RetentionDays:        30,
		ResultMaxBytes:       100 << 20,
		ArtifactMaxBytes:     10 << 30,
		LogSegmentMaxBytes:   200 << 20,
		LogTailLines:         2000,
		ReconcileInterval:    30 * time.Second,
		Kubeconfig:           env("AG_KUBECONFIG", ""),
		K8sInCluster:         envBool("AG_K8S_IN_CLUSTER", true),
		ClustersDir:          env("AG_CLUSTERS_DIR", "/etc/argus/clusters"),
		MetricsEnabled:       envBool("AG_METRICS_ENABLED", true),
		VolcanoEnabled:       envBool("AG_VOLCANO_ENABLED", true),
		VolcanoTTL:           15 * time.Second,
		DBMaxConns:           20,
		SSEHeartbeat:         15 * time.Second,
		SSEReplayWindow:      24 * time.Hour,
		AuthMode:             strings.ToLower(env("AG_AUTH_MODE", "dev")),
		Env:                  strings.ToLower(env("AG_ENV", "dev")),
		OIDCIssuer:           strings.TrimRight(env("AG_OIDC_ISSUER", ""), "/"),
		OIDCClientID:         env("AG_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:     env("AG_OIDC_CLIENT_SECRET", ""),
		OIDCClientAuth:       strings.ToLower(env("AG_OIDC_CLIENT_AUTH", "basic")),
		OIDCRedirectURL:      env("AG_OIDC_REDIRECT_URL", ""),
		OIDCScopes:           splitList(env("AG_OIDC_SCOPES", "openid profile email groups")),
		OIDCRoleClaim:        env("AG_OIDC_ROLE_CLAIM", "groups"),
		SessionSecret:        []byte(env("AG_SESSION_KEY", "")),
		SessionCookie:        env("AG_SESSION_COOKIE", "ag_session"),
		SessionTTL:           8 * time.Hour, // 下面按 AG_SESSION_TTL 覆盖
		SessionSecure:        envBool("AG_SESSION_SECURE", true),
		RoleMap:              parseRoleMap(env("AG_ROLE_MAP", "")),
		JobGCEnabled:         envBool("AG_JOB_GC_ENABLED", true),
		APIPrefix:            "/api/v1",
	}
	shared, smErr := ParseSharedMounts(env("AG_SHARED_MOUNTS", "[]"))
	if smErr != nil {
		return nil, smErr
	}
	c.SharedMounts = shared

	if raw := env("AG_SESSION_TTL", ""); raw != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: AG_SESSION_TTL=%q 非法（如 8h）", raw)
		}
		c.SessionTTL = d
	}

	if raw := env("AG_VOLCANO_TTL", ""); raw != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: AG_VOLCANO_TTL=%q 非法（如 15s）", raw)
		}
		c.VolcanoTTL = d
	}
	if raw := env("AG_RECONCILE_INTERVAL", ""); raw != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config: AG_RECONCILE_INTERVAL=%q 非法（如 30s）", raw)
		}
		c.ReconcileInterval = d
	}
	c.VolcanoNamespaces = volcanoNamespacesFromEnv()

	if c.Role == "" {
		c.Role = RoleAll
	}
	if !c.Role.Valid() {
		return nil, fmt.Errorf("config: AG_ROLE=%q 非法，允许 api|controller|collector|all", c.Role)
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("config: 缺少 AG_DB_DSN")
	}
	if _, err := url.Parse(c.DatabaseURL); err != nil {
		return nil, fmt.Errorf("config: AG_DB_DSN 非法: %w", err)
	}
	if c.StorageRoot == "" {
		return nil, errors.New("config: AG_STORAGE_ROOT 不能为空")
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}

	var err error
	if c.DBMaxConns, err = envInt32("AG_DB_MAX_OPEN_CONNS", c.DBMaxConns); err != nil {
		return nil, err
	}
	if c.MaxGlobalConcurrency, err = envInt("AG_CONCURRENCY_MAX", "", c.MaxGlobalConcurrency); err != nil {
		return nil, err
	}
	if c.TaskMaxRunners, err = envInt("AG_TASK_MAX_RUNNERS", "", c.TaskMaxRunners); err != nil {
		return nil, err
	}
	if c.AttemptMax, err = envInt("AG_ATTEMPT_MAX", "", c.AttemptMax); err != nil {
		return nil, err
	}
	if c.RetentionDays, err = envInt("AG_RETENTION_DAYS", "", c.RetentionDays); err != nil {
		return nil, err
	}
	if c.LogTailLines, err = envInt("AG_LOG_TAIL_LINES", "", c.LogTailLines); err != nil {
		return nil, err
	}
	if c.DeployTimeout, err = envDuration("AG_DEPLOY_TIMEOUT", "", c.DeployTimeout); err != nil {
		return nil, err
	}
	if c.RunnerTimeoutDefault, err = envDuration("AG_RUNNER_TIMEOUT", "", c.RunnerTimeoutDefault); err != nil {
		return nil, err
	}
	if c.RunnerTimeoutMax, err = envDuration("AG_RUNNER_TIMEOUT_MAX", "", c.RunnerTimeoutMax); err != nil {
		return nil, err
	}
	if c.ResultMaxBytes, err = envBytes("AG_RESULT_MAX_BYTES", c.ResultMaxBytes); err != nil {
		return nil, err
	}
	if c.ArtifactMaxBytes, err = envBytes("AG_ARTIFACT_MAX_BYTES", c.ArtifactMaxBytes); err != nil {
		return nil, err
	}
	if c.LogSegmentMaxBytes, err = envBytes("AG_LOG_SEGMENT_MAX_BYTES", c.LogSegmentMaxBytes); err != nil {
		return nil, err
	}
	if c.SSEHeartbeat, err = envDuration("AG_SSE_HEARTBEAT", "", c.SSEHeartbeat); err != nil {
		return nil, err
	}
	if c.SSEReplayWindow, err = envDurationHours("AG_SSE_REPLAY_HOURS", 24*time.Hour); err != nil {
		return nil, err
	}

	// 范围校验
	if c.MaxGlobalConcurrency < 1 || c.MaxGlobalConcurrency > 10000 {
		return nil, fmt.Errorf("config: AG_CONCURRENCY_MAX=%d 超出范围 [1,10000]", c.MaxGlobalConcurrency)
	}
	if c.TaskMaxRunners < 1 || c.TaskMaxRunners > 1000 {
		return nil, fmt.Errorf("config: AG_TASK_MAX_RUNNERS=%d 超出范围 [1,1000]", c.TaskMaxRunners)
	}
	if c.AttemptMax < 1 || c.AttemptMax > 100 {
		return nil, fmt.Errorf("config: AG_ATTEMPT_MAX=%d 超出范围 [1,100]", c.AttemptMax)
	}
	if c.RunnerTimeoutDefault <= 0 || c.RunnerTimeoutDefault > c.RunnerTimeoutMax {
		return nil, fmt.Errorf("config: 默认 runner 超时 %s 必须 >0 且 ≤ 上限 %s", c.RunnerTimeoutDefault, c.RunnerTimeoutMax)
	}
	// Profile CHECK 约束：deploy_timeout_seconds BETWEEN 3600 AND 604800。
	if c.DeployTimeout < time.Hour || c.DeployTimeout > 7*24*time.Hour {
		return nil, fmt.Errorf("config: 部署窗口 %s 超出范围 [1h,168h]（03 §3.2 的 CHECK 约束）", c.DeployTimeout)
	}
	if c.DBMaxConns < 1 {
		return nil, fmt.Errorf("config: AG_DB_MAX_OPEN_CONNS=%d 必须 ≥1", c.DBMaxConns)
	}
	if c.ResultMaxBytes < 1 {
		return nil, errors.New("config: AG_RESULT_MAX_BYTES 必须 >0")
	}
	if c.ArtifactMaxBytes < 1 {
		return nil, errors.New("config: AG_ARTIFACT_MAX_BYTES 必须 >0")
	}
	if c.RetentionDays < 1 {
		return nil, errors.New("config: AG_RETENTION_DAYS 必须 >0")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("config: AG_LOG_LEVEL=%q 非法，允许 debug|info|warn|error", c.LogLevel)
	}
	if c.OIDCClientAuth != "basic" && c.OIDCClientAuth != "post" {
		return nil, fmt.Errorf("config: AG_OIDC_CLIENT_AUTH=%q 非法，允许 basic|post", c.OIDCClientAuth)
	}
	if c.AuthMode == "oidc" {
		var missing []string
		if c.OIDCIssuer == "" {
			missing = append(missing, "AG_OIDC_ISSUER")
		}
		if c.OIDCClientID == "" {
			missing = append(missing, "AG_OIDC_CLIENT_ID")
		}
		if c.OIDCRedirectURL == "" {
			missing = append(missing, "AG_OIDC_REDIRECT_URL")
		}
		if len(c.SessionSecret) < 32 {
			missing = append(missing, "AG_SESSION_KEY(≥32 字节)")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("config: AG_AUTH_MODE=oidc 缺少必需配置: %s", strings.Join(missing, ", "))
		}
	}

	switch c.AuthMode {
	case "dev":
		// 红线（06 §6.1、07 §2.1）：dev 鉴权等于"任何人以任意身份操作"，
		// 只允许在**显式声明的本地环境**下启用；其它任何取值都启动即失败。
		switch c.Env {
		case "dev", "local":
		default:
			return nil, fmt.Errorf(
				"config: AG_AUTH_MODE=dev 只能与 AG_ENV=dev|local 一起用（当前 AG_ENV=%q）；正式环境请设 AG_AUTH_MODE=oidc",
				c.Env)
		}
	case "oidc":
	default:
		return nil, fmt.Errorf("config: AG_AUTH_MODE=%q 非法，允许 dev|oidc", c.AuthMode)
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

// TaskStorageDir 返回单个 Task 的产物目录（绝对路径）。
// 产物布局：`<storage-root>/<task_id>/attempts/<n>/...`（§8.1）。
func (c *Config) TaskStorageDir(taskID string) string {
	return joinStorage(c.StorageRoot, taskID)
}

func joinStorage(root string, parts ...string) string {
	out := strings.TrimRight(root, `/\`)
	for _, p := range parts {
		out += "/" + p
	}
	return out
}

// volcanoNamespacesFromEnv 解析 AG_VOLCANO_NAMESPACES：
//   - 未配置 → 默认 executor namespace（与 Job 所在 namespace 同值）；
//   - `*` / `all` → nil（全部 namespace，一次集群级 List）；
//   - 否则逗号/空白分隔的列表。
func volcanoNamespacesFromEnv() []string {
	raw := strings.TrimSpace(env("AG_VOLCANO_NAMESPACES", ""))
	if raw == "" {
		return []string{env("AG_EXECUTOR_NAMESPACE", "argus")}
	}
	if raw == "*" || strings.EqualFold(raw, "all") {
		return nil
	}
	if list := splitList(raw); len(list) > 0 {
		return list
	}
	return nil
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

// parseRoleMap 解析 AG_ROLE_MAP（`ag-admins=admin,ag-ops=operator`）。
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

// ---- env 读取 helper ------------------------------------------------------

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envInt(primary, alias string, def int) (int, error) {
	raw := env(primary, "")
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
	raw := env(primary, "")
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

// ParseSharedMounts 解析 AG_SHARED_MOUNTS（JSON 数组，D-63/D-65）。
//
// 形态：
//
//	[{"name":"models","mountPath":"/models","readOnly":true,"claimName":"argus-models"},
//	 {"name":"model-alt","mountPath":"/mnt/model","claimName":"argus-models"}]
//
// 规则：claimName 必填；mountPath 绝对且不与平台保留路径冲突；name 唯一；readOnly **默认 true**
// （只有显式写 false 才可写）。空/`[]` 表示不注入。
func ParseSharedMounts(raw string) ([]domain.SharedMountSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var items []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  *bool  `json:"readOnly"` // 指针：区分"未写（默认 true）"与"显式 false"
		ClaimName string `json:"claimName"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("config: AG_SHARED_MOUNTS 不是合法 JSON 数组: %w", err)
	}
	out := make([]domain.SharedMountSpec, 0, len(items))
	for _, it := range items {
		ro := true
		if it.ReadOnly != nil {
			ro = *it.ReadOnly
		}
		out = append(out, domain.SharedMountSpec{
			Name: it.Name, MountPath: it.MountPath, ReadOnly: ro, ClaimName: it.ClaimName,
		})
	}
	normalized, err := domain.NormalizeSharedMounts(out)
	if err != nil {
		return nil, fmt.Errorf("config: AG_SHARED_MOUNTS: %w", err)
	}
	return normalized, nil
}

// SharedMountReservedPaths 返回"平台保留 + 统一挂载"路径集合（提交期冲突校验用，D-63 ⑤）。
func (c *Config) SharedMountReservedPaths() map[string]string {
	return domain.ReservedMountPaths(c.SharedMounts)
}
