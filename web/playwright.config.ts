import { defineConfig } from '@playwright/test'

/**
 * 最小 Playwright 冒烟：本地跑，不引入 CI 依赖。
 *
 * 默认针对 Vite dev server（5173），由 webServer 自动拉起并把 /api 代理到
 * TESTPLANE_BACKEND（默认 http://127.0.0.1:8080）。
 * 也可以先起好服务，然后：
 *   PLAYWRIGHT_BASE_URL=http://127.0.0.1:8098 pnpm exec playwright test
 * 此时不再自动起 dev server（dist 被 embed 的形态）。
 *
 * 浏览器用系统 Chrome（channel: chrome），避免额外下载 Playwright Chromium。
 */
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? 'http://127.0.0.1:5173'
const useWebServer = !process.env.PLAYWRIGHT_BASE_URL

export default defineConfig({
  testDir: './tests',
  timeout: 45_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  reporter: [['line']],
  use: {
    baseURL,
    channel: process.env.PLAYWRIGHT_CHANNEL ?? 'chrome',
    headless: true,
    trace: 'retain-on-failure',
  },
  webServer: useWebServer
    ? {
        command: 'pnpm dev',
        url: 'http://127.0.0.1:5173',
        reuseExistingServer: true,
        timeout: 120_000,
        env: {
          TESTPLANE_BACKEND: process.env.TESTPLANE_BACKEND ?? 'http://127.0.0.1:8080',
          // UI 冒烟默认跑 mock：不依赖真后端、可离线；要打真后端用
          //   VITE_USE_MOCK=0 pnpm exec playwright test
          VITE_USE_MOCK: process.env.VITE_USE_MOCK ?? '1',
        },
      }
    : undefined,
})
