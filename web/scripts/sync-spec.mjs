// 把唯一机读契约（server/api/openapi.yaml）同步到 Web 侧，供 /api 页在运行时解析。
//
// 为什么要复制：Vite 只能打包 root（web/）内的资源，而契约的**唯一来源**在 server/ 下。
// 复制过来的是一份"单向投影"：手改会在下次 predev/prebuild 被覆盖，所以页面永远和契约一致。
//
// 来源按顺序找，找到第一个就用：
//   1. $TESTPLANE_OPENAPI           显式指定（CI / 特殊布局）
//   2. ../server/api/openapi.yaml   本地开发（仓库里直接跑）
//   3. ./server-api/openapi.yaml    Docker 前端阶段（只 COPY 了 web/ + 这一个文件）
//   4. src/generated/openapi.yaml   已生成过（离线构建）：沿用并打 warn，不静默
// 都找不到 → 退出码 1，并给出可操作的提示。
//
// 用法：node scripts/sync-spec.mjs（`pnpm dev` / `pnpm build` 前由 predev / prebuild 自动跑）
import { copyFileSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const webRoot = resolve(here, '..')
const targetDir = join(webRoot, 'src/generated')
const target = join(targetDir, 'openapi.yaml')

/** @type {Array<{ path: string, label: string, fallback?: boolean }>} */
const CANDIDATES = [
  ...(process.env.TESTPLANE_OPENAPI
    ? [{ path: resolve(webRoot, process.env.TESTPLANE_OPENAPI), label: '$TESTPLANE_OPENAPI' }]
    : []),
  { path: resolve(webRoot, '../server/api/openapi.yaml'), label: '仓库内契约（server/api/openapi.yaml）' },
  { path: join(webRoot, 'server-api/openapi.yaml'), label: 'Docker 构建阶段拷贝（./server-api/openapi.yaml）' },
  { path: target, label: '上一次生成的产物（src/generated/openapi.yaml）', fallback: true },
]

const found = CANDIDATES.find((candidate) => existsSync(candidate.path))

if (!found) {
  const tried = CANDIDATES.map((candidate) => `  - ${candidate.label}: ${candidate.path}`).join('\n')
  console.error(
    [
      '[sync-spec] 找不到 openapi.yaml，无法生成 src/generated/openapi.yaml。',
      '已尝试：',
      tried,
      '',
      '怎么办：',
      '  - 在仓库根目录（不是 web/）运行本脚本：node web/scripts/sync-spec.mjs；',
      '  - 或者用环境变量显式指定：TESTPLANE_OPENAPI=/path/to/server/api/openapi.yaml node scripts/sync-spec.mjs；',
      '  - 或者先把契约放到 web/server-api/openapi.yaml（Docker 前端阶段就是这么做的）。',
    ].join('\n'),
  )
  process.exit(1)
}

if (found.fallback) {
  console.warn(
    `[sync-spec] WARN: 没找到源码里的契约，沿用已有产物 ${relative(webRoot, found.path)}。` +
      '内容可能不是最新的；请在仓库根目录重跑一次以刷新。',
  )
  if (!existsSync(target)) process.exit(1)
} else {
  mkdirSync(targetDir, { recursive: true })
  copyFileSync(found.path, target)

  // 顺手写一个"别手改"的说明，避免有人以为这玩意儿该提交
  writeFileSync(
    join(targetDir, 'README.md'),
    [
      '此目录由 `scripts/sync-spec.mjs` 生成，不要手改。',
      '源文件：server/api/openapi.yaml（唯一机读契约）。',
      '`pnpm dev` / `pnpm build` 前会通过 predev / prebuild 自动同步。',
      '',
    ].join('\n'),
    'utf8',
  )

  const kb = (statSync(target).size / 1024).toFixed(1)
  const ops = (readFileSync(target, 'utf8').match(/^ {4}(get|post|put|patch|delete):/gm) ?? []).length
  console.log(`[sync-spec] ${relative(webRoot, target)} ← ${found.label} (${kb} KB, ${ops} 个操作)`)
}
