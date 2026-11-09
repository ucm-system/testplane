package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ucm-system/testplane/server/internal/auth"
	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// TesterResolver 是"Tester 目录"的消费方接口（Go 惯例：接口由消费方声明）。
// 实现由 registry 包提供（扫描 TESTERS_DIR 并入库），Lead 负责接线。
//
// 约定：找不到时返回包装了 domain.ErrNotFound 的错误（api 用 errors.Is 判 404/400）。
type TesterResolver interface {
	Resolve(ctx context.Context, name, version string) (domain.TesterSpec, error)
	List(ctx context.Context, role string, includeOrphaned bool) ([]domain.TesterSpec, error)
}

// Deps 是 API 的装配输入。
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Machines *store.Machines
	Resolver TesterResolver
	Logger   *slog.Logger
	// WebUI 是前端产物（webui.FS()）；nil 表示不挂静态站点。
	WebUI fs.FS
	// Version 是二进制版本（/meta 用）。
	Version string
	// Volcano 可选：不设时 /queue-status 的 volcano 块降级为 scheduler_unavailable。
	Volcano VolcanoProvider
	// Clock 是业务时间来源（05 §6.5）；nil 时用 domain.SystemClock。
	// 提交时间戳、run_id 日期段、resolved 快照时间都走它（票 #31）。
	Clock domain.Clock
}

// Server 持有 HTTP 层依赖。
type Server struct {
	cfg      *config.Config
	st       *store.Store
	machines *store.Machines
	resolver TesterResolver
	logger   *slog.Logger
	submit   *Submitter
	webui    fs.FS
	volcano  VolcanoProvider
	metrics  *metricsRegistry
	// deployLogsKube 是 Deploy 日志端点的 K8s 客户端（懒加载；测试可直接注入 fake）。
	deployLogsKube deployLogClient
	clock          domain.Clock
	authn          *auth.Authenticator
	version        string
	now            func() time.Time
}

// New 构造 API Server。
func New(d Deps) *Server {
	lg := d.Logger
	if lg == nil {
		lg = slog.Default()
	}
	s := &Server{
		cfg:      d.Config,
		st:       d.Store,
		machines: d.Machines,
		resolver: d.Resolver,
		logger:   lg,
		webui:    d.WebUI,
		volcano:  d.Volcano,
		version:  d.Version,
		now:      func() time.Time { return time.Now().UTC() },
		metrics:  newMetricsRegistry(),
	}
	s.clock = d.Clock
	if s.clock == nil {
		s.clock = domain.SystemClock
	}
	// 业务时间统一取注入时钟（票 #31）：提交时间戳、/meta.generated_at、SSE 回放窗口
	// 都走它；runtime 机制（Ticker/Timeout）仍用 time.Now()。
	s.now = s.clock.Now
	s.authn = s.buildAuthenticator()
	s.submit = &Submitter{
		resolver:       d.Resolver,
		profiles:       d.Store.Profiles,
		logger:         lg,
		clock:          s.clock,
		SharedMounts:   d.Config.SharedMounts,
		MaxTasks:       d.Config.RunMaxTasks,
		DefaultTimeout: d.Config.TaskTimeoutDefault,
		MaxTimeout:     d.Config.TaskTimeoutMax,
	}
	return s
}

// Handler 组装路由（07 §5 的端点表；MVP 子集由 Lead 定稿）。
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.middlewareRequestID)
	r.Use(s.middlewareRecover)
	r.Use(s.middlewareAccessLog)
	r.Use(s.middlewareMetrics)

	// 运维端点不带版本（07 §1）；/metrics 只读，且不参与静态 fallback。
	//
	// **免鉴权是有意的**：这三个端点注册在根路由上，而 middlewareAuth 只挂在
	// `Route(APIPrefix)` 组内（见下方），所以切到 OIDC 模式后 /metrics 依然公开，
	// 供 Prometheus 无需令牌抓取。改动路由结构时注意别把它挪进鉴权组。
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Get("/metrics", s.handleMetrics)

	// 认证端点（匿名）：07 §2.4。放在根路由，不经过 API 前缀的鉴权/CSRF 中间件。
	r.Get("/auth/login", s.handleAuthLogin)
	r.Get("/auth/callback", s.handleAuthCallback)
	r.Post("/auth/logout", s.handleAuthLogout)

	r.Route(s.cfg.APIPrefix, func(r chi.Router) {
		r.Use(s.middlewareAuth)

		r.Get("/meta", s.handleMeta)
		r.Get("/me", s.handleMe)

		// 写操作：operator 及以上（07 §2.3）。
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(authRoleOperator))
			r.Post("/runs", s.handleSubmitRun)
			r.Post("/runs/{id}/cancel", s.handleCancelRun)
			r.Post("/tasks/{id}/cancel", s.handleCancelTask)
			// 重试（07 §5.1/§8）：retry 创建新 Run（retry_of=原 Run），task retry 重开 Attempt。
			r.Post("/runs/{id}/retry", s.handleRetryRun)
			r.Post("/tasks/{id}/retry", s.handleRetryTask)
		})

		// 管理面（07 §5.4）：全部要求 admin。写操作同事务/事后写 audit_log（C-10）。
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(authRoleAdmin))
			r.Post("/admin/profiles", s.handleAdminCreateProfile)
			r.Put("/admin/profiles", s.handleAdminUpdateProfile)
			r.Delete("/admin/profiles", s.handleAdminDeleteProfile)
			r.Post("/admin/clusters", s.handleAdminCreateCluster)
			r.Put("/admin/clusters", s.handleAdminUpdateCluster)
			r.Delete("/admin/clusters", s.handleAdminDeleteCluster)
			r.Get("/admin/secrets", s.handleAdminListSecretRefs)
			r.Post("/admin/secrets", s.handleAdminCreateSecretRef)
			r.Post("/admin/secrets/{name}/rotate", s.handleAdminRotateSecret)
			r.Get("/admin/audit", s.handleAdminListAudit)
			r.Delete("/admin/runs/{run_id}/artifacts", s.handleAdminCleanupArtifacts)
		})

		// Secret 引用只读列表（07 §5.3）：**operator 及以上**（07 §2.3 / C-8.2：
		// "有哪些 Secret 被平台引用"本身是运维信息）；只回名称与说明，不回显值。
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(authRoleOperator))
			r.Get("/secrets", s.handleListSecretRefs)
		})

		// Run（只读：viewer）
		r.Get("/runs", s.handleListRuns)
		// 列筛选候选值（Excel 式表头下拉，D-61）。必须注册在 /runs/{id} 之前更直观
		// （chi 本身静态段优先，不依赖顺序）。
		r.Get("/runs/facets", s.handleRunFacets)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Get("/runs", s.handleListRuns)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Get("/runs/{id}/tasks", s.handleListRunTasks)
		r.Get("/runs/{id}/events", s.handleRunEvents)
		r.Get("/runs/{id}/events/history", s.handleRunEventsHistory)
		r.Get("/runs/{id}/report", s.handleRunReport)
		r.Get("/runs/{id}/report.md", s.handleRunReportMarkdown)
		r.Get("/runs/{id}/manifest", s.handleRunManifest)
		r.Get("/runs/{id}/deploy", s.handleRunDeploy)
		r.Get("/runs/{id}/deploy/logs", s.handleRunDeployLogs)
		r.Get("/runs/{id}/retry-plan", s.handleRetryPlan)
		r.Get("/runs/{id}/artifacts", s.handleRunArtifacts)
		// 打包下载（issue #41 / D-56）：一次拿全 report/events/artifacts/logs。
		r.Get("/runs/{id}/bundle", s.handleRunBundle)
		r.Get("/runs/{id}/files", s.handleRunFile)

		// Task / Attempt
		r.Get("/tasks", s.handleListTasks)
		r.Get("/tasks/{id}", s.handleGetTask)
		r.Get("/tasks/{id}/attempts", s.handleListAttempts)
		r.Get("/tasks/{id}/attempts/{idx}/result", s.handleAttemptResult)
		r.Get("/tasks/{id}/attempts/{idx}/logs", s.handleAttemptLogs)
		r.Get("/tasks/{id}/attempts/{idx}/artifacts", s.handleAttemptArtifacts)

		// Tester 目录（只读；Tester 随镜像发布，不做运行时注册，K-3）
		r.Get("/testers", s.handleListTesters)
		r.Get("/testers/{name}", s.handleGetTester)
		r.Get("/testers/{name}/versions/{version}", s.handleGetTesterVersion)

		r.Get("/profiles", s.handleListProfiles)
		r.Get("/clusters", s.handleListClusters)
		r.Get("/queue-status", s.handleQueueStatus)
	})

	// 静态前端：只挂根路径，API 前缀不 fallback（05 §5.3、W-7）。
	if s.webui != nil {
		r.Handle("/*", s.staticHandler())
	}
	return r
}

// ---- 中间件 --------------------------------------------------------------

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (s *Server) middlewareRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func (s *Server) middlewareRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered", "panic", rec, "request_id", RequestID(r.Context()),
					"path", r.URL.Path)
				fail(w, r, http.StatusInternalServerError, domain.CodeInternalError,
					"内部错误", nil, false)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 记录状态码与字节数，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush 让 SSE 能用 ResponseWriter.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// middlewareAccessLog 按 05 §6.4 的字段记访问日志。
func (s *Server) middlewareAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			s.logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("request_id", RequestID(r.Context())),
				slog.String("user", User(r.Context())),
				slog.String("role", Role(r.Context())),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// middlewareAuth 是 MVP 的 dev 鉴权（07 §2 的生产 OIDC 本轮不实现）：
//   - TESTPLANE_AUTH_MODE=dev（默认）：
//   - Authorization: Bearer <subject> → 用户名取 subject；不带则用户名 dev；
//   - 角色固定 admin（MVP 不做 RBAC 拒绝）。
//
// 生产 OIDC 留配置位：AuthMode=oidc 时启动打印未实现告警，仍按 dev 处理。

// staticHandler 提供前端产物；未知路径 fallback 到 index.html（SPA 路由）。
func (s *Server) staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.webui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, s.cfg.APIPrefix) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/" {
			if f, err := s.webui.Open(strings.TrimPrefix(r.URL.Path, "/")); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}
