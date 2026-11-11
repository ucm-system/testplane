// Command mock-llm 是一个 OpenAI 兼容的假 LLM 服务，用来喂 Argus 的
// endpoint-probe（真实 Tester）与 e2e 脚本：不需要真模型，就能复现
// "就绪 / 就绪慢 / 500 / 401 / 200 包错误" 这几种现场。
//
// 只用标准库，零依赖。默认监听 :18080（控制面是 8080，不要占）。
//
// 启动：
//
//	go run ./tools/mock-llm
//	go run ./tools/mock-llm -ready-after=3s -fail=500
//
// 行为：
//
//	GET  /healthz             进程存活，永远 200
//	GET  /v1/models           {"data":[{"id":"mock-llm"}]}
//	POST /v1/chat/completions stream:true → SSE（逐 token 的 data: 帧，最后 data: [DONE]）
//	                          stream:false → 普通 JSON，choices[0].message.content 非空
//
// 控制参数（查询参数优先，环境变量设默认值）：
//
//	?ready_after=3            服务启动后前 3 秒返回 503（模拟"还没起来"）
//	?fail=500 / ?fail=401     直接返回该 HTTP 状态
//	?error_in_200=1           HTTP 200，但响应体是 error 对象
//	?slow_ms=2000             响应前额外延迟 2 秒（模拟慢模型/慢首包）
//	?model=other              临时改返回的模型名
//
// 环境变量：MOCK_LLM_ADDR / MOCK_LLM_MODEL / MOCK_LLM_READY_AFTER / MOCK_LLM_FAIL /
// MOCK_LLM_ERROR_IN_200 / MOCK_LLM_SLOW_MS / MOCK_LLM_QUIET。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		addr = flag.String("addr", envString("MOCK_LLM_ADDR", ":18080"),
			"监听地址（默认 :18080；控制面在 8080，别占）")
		model = flag.String("model", envString("MOCK_LLM_MODEL", defaultModel),
			"返回的模型 id")
		readyAfter = flag.Duration("ready-after", envDuration("MOCK_LLM_READY_AFTER", 0),
			"启动后前 N 秒返回 503（?ready_after 的默认值）")
		failStatus = flag.Int("fail", envInt("MOCK_LLM_FAIL", 0),
			"默认注入的失败 HTTP 状态码，0 表示不注入（?fail 的默认值）")
		errorIn200 = flag.Bool("error-in-200", envBool("MOCK_LLM_ERROR_IN_200", false),
			"默认返回 HTTP 200 + error 对象（?error_in_200 的默认值）")
		slowMS = flag.Int("slow-ms", envInt("MOCK_LLM_SLOW_MS", 0),
			"默认响应前延迟毫秒数（?slow_ms 的默认值）")
		quiet = flag.Bool("quiet", envBool("MOCK_LLM_QUIET", false),
			"不打印每个请求")
	)

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `用法：mock-llm [参数]

OpenAI 兼容的假 LLM 服务，给 endpoint-probe 与 e2e 用。

参数：
`)
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), `
请求控制参数（查询参数，优先于启动参数/环境变量）：
  ?ready_after=3      服务启动后前 3 秒返回 503
  ?fail=500           直接返回 500（?fail=401 模拟认证失败）
  ?error_in_200=1     HTTP 200，但响应体是 error 对象
  ?slow_ms=2000       响应前额外延迟 2 秒
  ?model=other        临时改返回的模型名

示例：
  mock-llm -addr=:18080
  curl -N -X POST 'http://127.0.0.1:18080/v1/chat/completions?slow_ms=500' \
       -H 'Content-Type: application/json' \
       -d '{"model":"mock-llm","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":true}'
`)
	}
	flag.Parse()

	if *failStatus != 0 && (*failStatus < 100 || *failStatus > 599) {
		fmt.Fprintf(os.Stderr, "mock-llm: -fail 需要 100..599 的 HTTP 状态码，收到 %d\n", *failStatus)
		os.Exit(2)
	}
	if *slowMS < 0 {
		fmt.Fprintf(os.Stderr, "mock-llm: -slow-ms 不能为负，收到 %d\n", *slowMS)
		os.Exit(2)
	}

	cfg := Config{
		Model:      *model,
		ReadyAfter: *readyAfter,
		FailStatus: *failStatus,
		ErrorIn200: *errorIn200,
		Slow:       time.Duration(*slowMS) * time.Millisecond,
		Quiet:      *quiet,
	}
	s := NewServer(cfg)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout 必须为 0：SSE 是长连接，写超时会把流切断。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	publicAddr := *addr
	if strings.HasPrefix(publicAddr, ":") {
		publicAddr = "127.0.0.1" + publicAddr
	}
	fmt.Fprintf(os.Stderr, `mock-llm 已启动
  监听       %s
  base URL   http://%s/v1
  路由       GET /v1/models · POST /v1/chat/completions · GET /healthz
  默认故障   ready_after=%s fail=%d error_in_200=%v slow_ms=%d
  控制参数   ?ready_after=3 ?fail=500 ?fail=401 ?error_in_200=1 ?slow_ms=2000
  停止       Ctrl+C
`, *addr, publicAddr, cfg.ReadyAfter, cfg.FailStatus, cfg.ErrorIn200, *slowMS)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("mock-llm 关闭超时：%v", err)
		}
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "mock-llm 启动失败（%s 可能被占用）：%v\n", *addr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "mock-llm 已停止")
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		fmt.Fprintf(os.Stderr, "mock-llm: 环境变量 %s=%q 不是整数，忽略\n", key, v)
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := parseBool(v); err == nil {
			return b
		}
		fmt.Fprintf(os.Stderr, "mock-llm: 环境变量 %s=%q 不是布尔值，忽略\n", key, v)
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := parseSecondsOrDuration(v); err == nil && d >= 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "mock-llm: 环境变量 %s=%q 不是合法时长，忽略\n", key, v)
	}
	return fallback
}
