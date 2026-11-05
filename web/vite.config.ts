import { fileURLToPath, URL } from 'node:url'
import { defineConfig, type ProxyOptions } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

/**
 * TestPlane Web —— 开发服务器把 /api 代理到控制面（默认 127.0.0.1:8080）。
 * 换后端：`$env:TESTPLANE_BACKEND = 'http://127.0.0.1:9090'; pnpm dev`
 *
 * API 前缀固定 /api/v1（design/views/07-api.md §1）；/healthz、/readyz、/metrics 不带版本。
 * SSE：design/impl/web-and-delivery.md §7「开发代理」要求代理段关闭缓冲，
 * 否则 dev 环境下 text/event-stream 会被攒成一段再吐，事件流看起来像「卡住」。
 */
const BACKEND = process.env.TESTPLANE_BACKEND ?? 'http://127.0.0.1:8080'

/** 反代条目：禁缓冲 + no-cache + 不设读超时（SSE 是长连接）。 */
function proxyTo(prefix: string): ProxyOptions {
  return {
    target: BACKEND,
    changeOrigin: true,
    timeout: 0,
    proxyTimeout: 0,
    configure: (proxy) => {
      proxy.on('proxyRes', (proxyRes) => {
        if (prefix === '/api') {
          proxyRes.headers['cache-control'] = 'no-cache, no-transform'
          proxyRes.headers['x-accel-buffering'] = 'no'
        }
      })
    },
  }
}

export default defineConfig({
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
      '/api': proxyTo('/api'),
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
})
