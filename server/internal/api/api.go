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
	version  string
	now      func() time.Time
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
	}
	s.submit = &Submitter{
		resolver:       d.Resolver,
		profiles:       d.Store.Profiles,
		logger:         lg,
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

	// 运维端点不带版本（07 §1）。
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route(s.cfg.APIPrefix, func(r chi.Router) {
		r.Use(s.middlewareAuth)

		r.Get("/meta", s.handleMeta)
		r.Get("/me", s.handleMe)

		// Run
		r.Post("/runs", s.handleSubmitRun)
		r.Get("/runs", s.handleListRuns)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Get("/runs/{id}/tasks", s.handleListRunTasks)
		r.Post("/runs/{id}/cancel", s.handleCancelRun)
		r.Get("/runs/{id}/events", s.handleRunEvents)
		r.Get("/runs/{id}/events/history", s.handleRunEventsHistory)
		r.Get("/runs/{id}/report", s.handleRunReport)
		r.Get("/runs/{id}/report.md", s.handleRunReportMarkdown)
		r.Get("/runs/{id}/manifest", s.handleRunManifest)
		r.Get("/runs/{id}/deploy", s.handleRunDeploy)

		// Task / Attempt
		r.Get("/tasks", s.handleListTasks)
		r.Get("/tasks/{id}", s.handleGetTask)
		r.Post("/tasks/{id}/cancel", s.handleCancelTask)
		r.Get("/tasks/{id}/attempts", s.handleListAttempts)
		r.Get("/tasks/{id}/attempts/{idx}/result", s.handleAttemptResult)
		r.Get("/tasks/{id}/attempts/{idx}/logs", s.handleAttemptLogs)

		// Tester 目录（只读；Tester 随镜像发布，不做运行时注册，K-3）
		r.Get("/testers", s.handleListTesters)
		r.Get("/testers/{name}", s.handleGetTester)

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
func (s *Server) middlewareAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := "dev"
		if h := r.Header.Get("Authorization"); h != "" {
			parts := strings.SplitN(h, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				fail(w, r, http.StatusUnauthorized, domain.CodeUnauthenticated,
					"Authorization 头格式非法", nil, false)
				return
			}
			if sub := strings.TrimSpace(parts[1]); sub != "" {
				user = sub
			}
		}
		ctx := WithIdentity(r.Context(), user, "admin")
		next.ServeHTTP(w, r.WithContext(ctx))
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
