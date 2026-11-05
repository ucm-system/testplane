package genet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 传输层常量。
const (
	maxResponseBytes = 8 << 20 // 单响应上限：防止列表/富化响应把内存打满
	userAgent        = "testplane-controller"
)

// 传输层错误（供调用方区分"可重试"与"确定性失败"）。
var (
	// ErrReleaseNotFound 是 GET/DELETE 的 404：release 还没出现（异步 install 未落）。
	ErrReleaseNotFound = errors.New("genet: helm release not found")
	// ErrReleaseExists 是 POST 的同名 400/409：已存在（幂等路径由调用方 GET 复核）。
	ErrReleaseExists = errors.New("genet: helm release already exists")
	// ErrOperationInProgress 是 409：metadata 操作在飞，稍后重试即可。
	ErrOperationInProgress = errors.New("genet: helm operation in progress")
	// ErrUnauthorized 是 401/403：Key 无效 / scope 不足 / Key 未绑定 namespace / 时间窗拒绝。
	ErrUnauthorized = errors.New("genet: unauthorized")
	// ErrServer 是 5xx / 网络错误：平台侧问题，可重试但本轮判 DEPLOY_FAILED。
	ErrServer = errors.New("genet: server error")
)

// Transport 是最小 REST 传输层：鉴权、超时、CA/自签、错误映射。
type Transport struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

// TransportOptions 是传输层配置。
type TransportOptions struct {
	// BaseURL 形如 https://genet.dev.huawei.com（可带路径前缀）。
	BaseURL string
	// APIKey 是 Genet OpenAPI 的 Bearer Key（scope 需要 read+write）。
	APIKey string
	// CAFile 是自签 CA 的 PEM 路径；为空则用系统根证书。
	CAFile string
	// Insecure 跳过证书校验（仅开发/自签环境，默认 false）。
	Insecure bool
	// Timeout 是单请求超时（默认 10s）。
	Timeout time.Duration
	// HTTPClient 可直接注入（测试用 httptest.Client）。
	HTTPClient *http.Client
}

// NewTransport 构造传输层。
func NewTransport(opts TransportOptions) (*Transport, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("genet: base URL is required (TESTPLANE_GENET_BASE_URL)")
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("genet: parse base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("genet: base URL %q must be absolute (scheme://host)", opts.BaseURL)
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("genet: API key is required (TESTPLANE_GENET_API_KEY)")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		httpClient, err := buildHTTPClient(opts.CAFile, opts.Insecure, timeout)
		if err != nil {
			return nil, err
		}
		client = httpClient
	}
	return &Transport{baseURL: u, apiKey: strings.TrimSpace(opts.APIKey), httpClient: client}, nil
}

// buildHTTPClient 处理自签 CA 与 Insecure 选项。
func buildHTTPClient(caFile string, insecure bool, timeout time.Duration) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		// #nosec G402 —— 显式配置项（TESTPLANE_GENET_INSECURE），默认关闭。
		tlsCfg.InsecureSkipVerify = true
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("genet: read CA file %q: %w", caFile, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("genet: CA file %q has no valid PEM certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

// do 发一次请求并把非 2xx 映射成哨兵错误。
func (t *Transport) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, int, error) {
	u := *t.baseURL
	u.Path = strings.TrimRight(t.baseURL.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("genet: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, 0, fmt.Errorf("genet: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrServer, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: read body: %v", ErrServer, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, resp.StatusCode, nil
	}
	return raw, resp.StatusCode, mapStatusError(method, resp.StatusCode, raw)
}

// mapStatusError 把 HTTP 状态映射成哨兵错误；其余信息保留在错误文本里（排障用）。
func mapStatusError(method string, status int, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 512 {
		msg = msg[:512] + "…"
	}
	detail := fmt.Sprintf("%s %d: %s", method, status, msg)
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrReleaseNotFound, detail)
	case http.StatusConflict:
		if strings.Contains(msg, "in progress") {
			return fmt.Errorf("%w: %s", ErrOperationInProgress, detail)
		}
		return fmt.Errorf("%w: %s", ErrReleaseExists, detail)
	case http.StatusBadRequest:
		if strings.Contains(msg, "already has protected metadata") || strings.Contains(msg, "already exists") {
			return fmt.Errorf("%w: %s", ErrReleaseExists, detail)
		}
		return fmt.Errorf("%w: %s", ErrServer, detail)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrUnauthorized, detail)
	default:
		if status >= 500 {
			return fmt.Errorf("%w: %s", ErrServer, detail)
		}
		return fmt.Errorf("%w: %s", ErrServer, detail)
	}
}

// getJSON / postJSON / deleteJSON 是三个便利封装。
func (t *Transport) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	raw, _, err := t.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrServer, path, err)
	}
	return nil
}

func (t *Transport) postJSON(ctx context.Context, path string, body any, out any) (int, error) {
	raw, status, err := t.do(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return status, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, fmt.Errorf("%w: decode %s: %v", ErrServer, path, err)
		}
	}
	return status, nil
}

func (t *Transport) deleteJSON(ctx context.Context, path string, query url.Values, out any) error {
	raw, _, err := t.do(ctx, http.MethodDelete, path, query, nil)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: decode %s: %v", ErrServer, path, err)
		}
	}
	return nil
}
