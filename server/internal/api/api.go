// Package api 是 HTTP 边界：路由、中间件、鉴权、错误映射、SSE（v2：/api/v1 全表对齐
// server/api/openapi.yaml 的 51 路径 / 59 operations）。
//
// 规则（05 §3.1）：api 只做鉴权、校验、读写投影；不直接调 executor、不写 SQL，
// 状态推进一律通过 store 门面 / 编排层出口。所有业务端点在 /api/v1 下（07 §1），
// /healthz、/readyz、/metrics 不带版本。
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
	"github.com/ucm-system/testplane/server/internal/orchestrator"
	"github.com/ucm-system/testplane/server/internal/store"
)

// RunnerCatalog 是 Runner 注册表的只读消费面（实现：registry.Registry）。
// 提交组装（orchestrator.Assembler）与 dry-run 预览用。
type RunnerCatalog interface {
	Resolve(ctx context.Context, name, version string) (domain.RunnerSpec, error)
	List(ctx context.Context, role string, includeOrphaned bool) ([]domain.RunnerSpec, error)
}

// CleanReplayer 是编排层的泄漏重放出口（实现：*orchestrator.Scheduler）。
// 未接线（单 API 副本 / 测试）时 POST /tasks/{id}/clean 降级为 503 EXECUTOR_UNAVAILABLE。
type CleanReplayer interface {
	ReplayClean(ctx context.Context, taskID string, attemptNo int, force bool, actor, role string) (*orchestrator.ReplayCleanResult, error)
}

// Deps 是 API 的装配输入。
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Machines *store.Machines
	// Catalog 是 Runner 注册表（registry.Registry）；提交校验与 dry-run 必需。
	Catalog RunnerCatalog
	// CleanReplayer 是编排层的泄漏重放出口（可选；未接线时 clean 端点降级）。
	CleanReplayer CleanReplayer
	Logger        *slog.Logger
	// WebUI 是前端产物（webui.FS()）；nil 表示不挂静态站点。
	WebUI fs.FS
	// Version 是二进制版本（/meta 用）。
	Version string
	// Volcano 可选：不设时 /queue-status 的 volcano 块降级为 scheduler_unavailable。
	Volcano VolcanoProvider
	// Clock 是业务时间来源（05 §6.5）；nil 时用 domain.SystemClock。
	Clock domain.Clock
	// SharedMountsFn 是平台级统一共享挂载的**运行时来源**（D-65，读 shared_mounts 表）。
	SharedMountsFn func(ctx context.Context) ([]domain.SharedMountSpec, error)
}

// Server 持有 HTTP 层依赖。
type Server struct {
	cfg      *config.Config
	st       *store.Store
	machines *store.Machines
	catalog  RunnerCatalog
	replay   CleanReplayer
	logger   *slog.Logger
	assemble *orchestrator.Assembler
	webui    fs.FS
	volcano  VolcanoProvider
	metrics  *metricsRegistry
	clock    domain.Clock
	authn    *auth.Authenticator
	version  string
	now      func() time.Time
	// sharedMounts 是 D-65 的运行时挂载来源（nil → 用 cfg.SharedMounts）。
	sharedMounts func(ctx context.Context) ([]domain.SharedMountSpec, error)
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
		catalog:  d.Catalog,
		replay:   d.CleanReplayer,
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
	// 业务时间统一取注入时钟（票 #31）。
	s.now = s.clock.Now
	s.authn = s.buildAuthenticator()
	s.sharedMounts = d.SharedMountsFn
	if d.Catalog != nil {
		s.assemble = &orchestrator.Assembler{
			Catalog:       d.Catalog,
			ProfileExists: func(ctx context.Context, name string) (bool, error) { return s.st.Profiles.Exists(ctx, nil, name) },
			SecretExists: func(ctx context.Context, name string) (bool, error) {
				_, err := s.st.GetSecretRef(ctx, name)
				return err == nil, nil
			},
			DefaultTimeoutSeconds: int(s.cfg.RunnerTimeoutDefault / time.Second),
			MaxTimeoutSeconds:     int(s.cfg.RunnerTimeoutMax / time.Second),
			MaxRunners:            s.cfg.TaskMaxRunners,
		}
	}
	return s
}

// Handler 组装路由（server/api/openapi.yaml 的 59 operations）。
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.middlewareRequestID)
	r.Use(s.middlewareRecover)
	r.Use(s.middlewareAccessLog)
	r.Use(s.middlewareMetrics)

	// 运维端点不带版本（07 §1）；免鉴权是有意的（Prometheus 无需令牌抓取）。
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
			r.Post("/tasks", s.handleSubmitTask)
			r.Post("/tasks/{task_id}/cancel", s.handleCancelTask)
			r.Post("/tasks/{task_id}/retry", s.handleRetryTask)
			r.Post("/tasks/{task_id}/clean", s.handleReplayClean)
			r.Get("/secrets", s.handleListSecretRefs)
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
			r.Get("/admin/shared-mounts", s.handleAdminListSharedMounts)
			r.Post("/admin/shared-mounts", s.handleAdminCreateSharedMount)
			r.Put("/admin/shared-mounts/{name}", s.handleAdminUpdateSharedMount)
			r.Delete("/admin/shared-mounts/{name}", s.handleAdminDeleteSharedMount)
			r.Get("/admin/audit", s.handleAdminListAudit)
			r.Delete("/admin/tasks/{task_id}/artifacts", s.handleAdminCleanupArtifacts)
		})

		// Task（只读 + 事件）：viewer。
		r.Get("/tasks", s.handleListTasks)
		r.Get("/tasks/facets", s.handleTaskFacets)
		r.Get("/tasks/{task_id}", s.handleGetTask)
		r.Get("/tasks/{task_id}/report", s.handleTaskReport)
		r.Get("/tasks/{task_id}/report.md", s.handleTaskReportMarkdown)
		r.Get("/tasks/{task_id}/events", s.handleTaskEvents)
		r.Get("/tasks/{task_id}/events/history", s.handleTaskEventsHistory)
		r.Get("/tasks/{task_id}/manifest", s.handleTaskManifest)
		r.Get("/tasks/{task_id}/deploy", s.handleTaskDeploy)
		r.Get("/tasks/{task_id}/deploy/logs", s.handleTaskDeployLogs)
		r.Get("/tasks/{task_id}/runners", s.handleListTaskRunners)
		r.Get("/tasks/{task_id}/runners/{seq}", s.handleGetTaskRunner)
		r.Get("/tasks/{task_id}/runners/{seq}/result", s.handleTaskRunnerResult)
		r.Get("/tasks/{task_id}/runners/{seq}/logs", s.handleTaskRunnerLogs)
		r.Get("/tasks/{task_id}/runners/{seq}/artifacts", s.handleTaskRunnerArtifacts)
		r.Get("/tasks/{task_id}/attempts", s.handleListTaskAttempts)
		r.Get("/tasks/{task_id}/attempts/{attempt_no}/runners", s.handleListAttemptRunners)
		r.Get("/tasks/{task_id}/attempts/{attempt_no}/runners/{seq}", s.handleGetAttemptRunner)
		r.Get("/tasks/{task_id}/attempts/{attempt_no}/runners/{seq}/result", s.handleAttemptRunnerResult)
		r.Get("/tasks/{task_id}/attempts/{attempt_no}/runners/{seq}/logs", s.handleAttemptRunnerLogs)
		r.Get("/tasks/{task_id}/attempts/{attempt_no}/runners/{seq}/artifacts", s.handleAttemptRunnerArtifacts)
		r.Get("/tasks/{task_id}/artifacts", s.handleTaskArtifacts)
		r.Get("/tasks/{task_id}/bundle", s.handleTaskBundle)
		r.Get("/tasks/{task_id}/files", s.handleTaskFile)

		// Runner 目录（只读；随镜像发布，无运行时注册，K-3）。
		r.Get("/runners", s.handleListRunners)
		r.Get("/runners/{name}", s.handleGetRunner)
		r.Get("/runners/{name}/versions/{version}", s.handleGetRunnerVersion)

		r.Get("/profiles", s.handleListProfiles)
		r.Get("/clusters", s.handleListClusters)
		r.Get("/queue-status", s.handleQueueStatus)
		r.Get("/shared-mounts", s.handleListSharedMounts)
	})

	// 静态前端：只挂根路径，API 前缀不 fallback（05 §5.3、W-7）。
	if s.webui != nil {
		r.Handle("/*", s.staticHandler())
	}
	return r
}

// ---- 中间件 ---------------------------------------------------------------

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

// Flush 让 SSE 能用 ResponseWriter。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// middlewareAccessLog 按 05 §6.4 的字段记访问日志（task_id/attempt_no/runner_seq 语境字段
// 由各 handler 自行打业务日志）。
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
