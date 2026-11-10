import { fileURLToPath, URL } from 'node:url'
import { defineConfig, loadEnv, type ProxyOptions } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

/**
 * Argus Web —— 开发服务器把 /api 代理到控制面。
 *
 * 代理目标取值顺序：`.env` / `.env.local` 里的 `TESTPLANE_BACKEND` → 进程环境变量 → 默认 `http://127.0.0.1:8080`。
 * 端口转发到别处时（例如 `127.0.0.1:8081`）写进 `web/.env.local` 即可：
 *   TESTPLANE_BACKEND=http://127.0.0.1:8081
 *
 * API 前缀固定 /api/v1（design/views/07-api.md §1）；/healthz、/readyz、/metrics 不带版本。
 * SSE：design/impl/web-and-delivery.md §7「开发代理」要求代理段关闭缓冲，
 * 否则 dev 环境下 text/event-stream 会被攒成一段再吐，事件流看起来像「卡住」。
 */
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const backend = env.TESTPLANE_BACKEND || process.env.TESTPLANE_BACKEND || 'http://127.0.0.1:8080'

  /** 反代条目：禁缓冲 + no-cache + 不设读超时（SSE 是长连接）。 */
  const proxyTo = (prefix: string): ProxyOptions => ({
    target: backend,
    changeOrigin: true,
    timeout: 0,
    proxyTimeout: 0,
    configure: (proxy) => {
      proxy.on('proxyRes', (proxyRes) => {
        if (prefix.startsWith('/api')) {
          proxyRes.headers['cache-control'] = 'no-cache, no-transform'
          proxyRes.headers['x-accel-buffering'] = 'no'
        }
      })
    },
  })

  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    server: {
      host: '127.0.0.1',
      port: 5173,
      strictPort: true,
      proxy: {
        // 只代理**带版本前缀**的真实接口：/api/v1/…；
        // 否则会把站内页面 `/api`（API 参考）也一并转发出去，导致该路由 404/500。
        '/api/v1': proxyTo('/api/v1'),
        '/healthz': proxyTo('/healthz'),
        '/readyz': proxyTo('/readyz'),
        '/metrics': proxyTo('/metrics'),
      },
    },
    build: {
      // design/impl/web-and-delivery.md §1：内网 Chrome，无需 polyfill
      target: 'chrome110',
      outDir: 'dist',
      sourcemap: false,
    },
  }
})
