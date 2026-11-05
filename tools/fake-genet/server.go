package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 与真实 Genet 对齐的常量。
const (
	StatusPendingInstall = "pending-install"
	StatusDeployed       = "deployed"
	StatusUninstalled    = "uninstalled"

	EndpointPending = "pending"
	EndpointReady   = "ready"
	EndpointBlocked = "blocked"

	// 默认 mock-llm 的 Service 端口（chart 里固定 18080）。
	servicePort = 18080
)

// releaseNamePattern 是真实 Genet 的校验正则（handlers/helm.go:36）。
var releaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,52}$`)

// Server 是假 Genet 的 HTTP 面：只实现客户端真正会调的那几条（execution §9）。
type Server struct {
	Store     Store
	Namespace string // API Key 绑定的 namespace（不由请求指定）
	APIKey    string
	Model     string // callInfo.model
	// ServiceNameSuffix 是 chart 里 Service 的固定后缀（`<release>-mock-llm`）。
	ServiceNameSuffix string
	// SimBlocked 让 endpoint 永远"入口不可用"（模拟配额不足）：status=deployed 但 endpoint=blocked。
	SimBlocked bool
	// SimError 让所有 /api/open/helm/* 返回该 HTTP 状态（模拟 Genet 侧故障）。
	SimError int
	Now      func() time.Time
}

// NewServer 构造假 Genet。
func NewServer(store Store, namespace, apiKey, model string, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		Store: store, Namespace: namespace, APIKey: apiKey, Model: model,
		ServiceNameSuffix: "mock-llm", Now: now,
	}
}

// Handler 返回带鉴权的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "namespace": s.Namespace})
	})
	// 队列接口只为让客户端"best-effort 查询"不报错；本假实现不含 Volcano 数据。
	mux.HandleFunc("/api/open/queue", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
	}))
	mux.HandleFunc("/api/open/queue/podgroups", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "podGroups": []any{}})
	}))
	helm := func(next http.HandlerFunc) http.HandlerFunc {
		return s.auth(func(w http.ResponseWriter, r *http.Request) {
			if s.SimError != 0 {
				writeError(w, s.SimError, "simulated Genet failure")
				return
			}
			next(w, r)
		})
	}
	mux.HandleFunc("/api/open/helm/uc-packages/", helm(s.handleInstall))
	mux.HandleFunc("/api/open/helm/releases", helm(s.handleList))
	mux.HandleFunc("/api/open/helm/releases/", helm(s.handleRelease))
	return mux
}

// auth 校验 `Authorization: Bearer <key>`。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		want := "Bearer " + s.APIKey
		if s.APIKey != "" && got != want {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, r)
	}
}

// handleInstall 处理 POST /api/open/helm/uc-packages/{stack|pd}/releases。
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// 路径：/api/open/helm/uc-packages/<type>/releases
	rest := strings.TrimPrefix(r.URL.Path, "/api/open/helm/uc-packages/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "releases" {
		writeError(w, http.StatusNotFound, "unknown path")
		return
	}
	packageType := parts[0]
	if packageType != "stack" && packageType != "pd" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid package type %q", packageType))
		return
	}

	var body struct {
		ReleaseName  string `json:"releaseName"`
		Values       string `json:"values"`
		NoWait       bool   `json:"noWait"`
		TemplatePath string `json:"templatePath"`
		Set          []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			Type  string `json:"type"`
		} `json:"set"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.ReleaseName == "" {
		writeError(w, http.StatusBadRequest, "releaseName is required")
		return
	}
	if !releaseNamePattern.MatchString(body.ReleaseName) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("releaseName %q must match %s", body.ReleaseName, releaseNamePattern))
		return
	}

	// 同名已存在 → 400（真实 Genet 的语义，客户端会 GET 复核后当成功）。
	if _, err := s.Store.Status(r.Context(), body.ReleaseName); err == nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("release %q already has protected metadata; install cannot replace an existing release", body.ReleaseName))
		return
	} else if !errors.Is(err, ErrNotInstalled) {
		writeError(w, http.StatusInternalServerError, "probe release: "+err.Error())
		return
	}

	set := make([]string, 0, len(body.Set))
	for _, sv := range body.Set {
		set = append(set, sv.Name+"="+sv.Value)
	}
	// noWait=true 语义：不传 --wait，立即返回 202（排队/调度交给平台轮询）。
	if err := s.Store.Install(r.Context(), body.ReleaseName, body.Values, set); err != nil {
		if strings.Contains(err.Error(), "already has protected metadata") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "install: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name":          body.ReleaseName,
		"requestedName": body.ReleaseName,
		"namespace":     s.Namespace,
		"packageType":   packageType,
		"chart":         "mock-llm",
		"chartVersion":  "0.1.0",
		"status":        StatusPendingInstall,
	})
}

// handleList 处理 GET /api/open/helm/releases（支持 light=true）。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rels, err := s.Store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list: "+err.Error())
		return
	}
	light := r.URL.Query().Get("light") == "true"
	out := make([]map[string]any, 0, len(rels))
	for _, rel := range rels {
		item := map[string]any{
			"name": rel.Name, "namespace": s.Namespace, "status": rel.Status,
			"chart": rel.Chart, "chartVersion": rel.ChartVersion, "revision": rel.Revision,
		}
		if !light {
			if info, err := s.Store.Status(r.Context(), rel.Name); err == nil {
				item["pods"] = podsJSON(info.Pods)
				item["callInfo"] = s.callInfo(info)
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": out})
}

// handleRelease 处理 GET / DELETE /api/open/helm/releases/{name}。
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/open/helm/releases/")
	name = strings.Trim(name, "/")
	if name == "" {
		writeError(w, http.StatusNotFound, "release name required")
		return
	}
	if !releaseNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid release name")
		return
	}

	switch r.Method {
	case http.MethodGet:
		info, err := s.Store.Status(r.Context(), name)
		if err != nil {
			if errors.Is(err, ErrNotInstalled) {
				writeError(w, http.StatusNotFound, "helm release not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "status: "+err.Error())
			return
		}
		light := r.URL.Query().Get("light") == "true"
		resp := map[string]any{
			"name": info.Name, "namespace": s.Namespace,
			"status": info.Status, "description": info.Description, "revision": info.Revision,
			"chart": info.Chart, "chartVersion": info.ChartVersion,
		}
		if !light {
			resp["pods"] = podsJSON(info.Pods)
			resp["callInfo"] = s.callInfo(info)
		}
		writeJSON(w, http.StatusOK, resp)

	case http.MethodDelete:
		force := r.URL.Query().Get("force") == "true"
		keepHistory := r.URL.Query().Get("keepHistory") == "true"
		if err := s.Store.Uninstall(r.Context(), name); err != nil {
			if errors.Is(err, ErrNotInstalled) {
				writeError(w, http.StatusNotFound, "helm release not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "uninstall: "+err.Error())
			return
		}
		msg := fmt.Sprintf("release %q uninstalled", name)
		if force {
			msg += " (forced)"
		}
		if keepHistory {
			msg += " (history kept)"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"message": msg, "forcedPods": []string{}, "secretsDeleted": 0, "cleanupErrors": []string{},
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// callInfo 按调研里的形状合成 callInfo（endpoint 只在"deployed + Pod 全 Ready"时 ready）。
func (s *Server) callInfo(info ReleaseInfo) map[string]any {
	if s.SimBlocked {
		return map[string]any{
			"namespace":   s.Namespace,
			"model":       s.Model,
			"serviceName": fmt.Sprintf("%s-%s", info.Name, s.ServiceNameSuffix),
			"endpoint": map[string]any{
				"status":  EndpointBlocked,
				"source":  "service",
				"message": "insufficient quota (simulated)",
			},
		}
	}
	endpoint := map[string]any{
		"status":  EndpointPending,
		"source":  "service",
		"model":   s.Model,
		"message": "waiting for pods to become ready",
	}
	epStatus := EndpointPending
	url := ""
	if info.Status == StatusDeployed {
		if allReady(info.Pods) && len(info.Pods) > 0 {
			epStatus = EndpointReady
			url = fmt.Sprintf("http://%s-%s.%s.svc:%d/v1", info.Name, s.ServiceNameSuffix, s.Namespace, servicePort)
			endpoint["status"] = EndpointReady
			endpoint["url"] = url
			endpoint["host"] = fmt.Sprintf("%s-%s.%s.svc", info.Name, s.ServiceNameSuffix, s.Namespace)
			endpoint["port"] = servicePort
			endpoint["message"] = ""
		}
	} else if info.Status == "failed" {
		epStatus = EndpointBlocked
		endpoint["status"] = EndpointBlocked
		endpoint["message"] = info.Description
	}
	_ = epStatus
	return map[string]any{
		"namespace": s.Namespace,
		"model":     s.Model,
		"serviceName": fmt.Sprintf("%s-%s", info.Name, s.ServiceNameSuffix),
		"endpoint":  endpoint,
	}
}

func podsJSON(pods []Pod) []map[string]any {
	out := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		out = append(out, map[string]any{
			"name": p.Name, "phase": p.Phase, "nodeName": p.NodeName, "ready": p.Ready,
		})
	}
	return out
}

func allReady(pods []Pod) bool {
	for _, p := range pods {
		if !p.Ready {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message})
}
