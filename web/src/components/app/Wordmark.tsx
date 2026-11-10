import { useId } from 'react'
import { cn } from '@/lib/utils'

/**
 * 品牌字标（`design/prototype/index.html` 的 `<svg class="hg-wordmark">` 原样搬进 React）。
 *
 * 三条纪律：
 *  - 图形 + 字母是一体的身份标识，**不要**再在外面配 `<img favicon>` + `<span>Argus</span>`；
 *  - 原型里的 `linearGradient` id 是全局的，这里用 `useId()` 生成唯一 id，避免同页多实例串色；
 *  - 字色走 `currentColor`（`text-fg`），跟随主题，不写死 hex。
 */
export function Wordmark({ className, height = 26 }: { className?: string; height?: number }) {
  const gradientId = useId().replace(/[:]/g, '')
  return (
    <svg
      viewBox="0 0 134 30"
      role="img"
      aria-label="Argus"
      className={cn('w-auto shrink-0 overflow-visible', className)}
      style={{ height }}
    >
      <defs>
        <linearGradient id={gradientId} x1="128" y1="362" x2="386" y2="142" gradientUnits="userSpaceOnUse">
          <stop stopColor="#22D3EE" />
          <stop offset="0.52" stopColor="#38BDF8" />
          <stop offset="1" stopColor="#6366F1" />
        </linearGradient>
      </defs>
      <g transform="translate(0,2) scale(0.0508)">
        <rect x="16" y="16" width="480" height="480" rx="112" fill="#0B1220" />
        <path d="M118 319L255 246L394 319L256 392L118 319Z" fill="#172B43" />
        <path d="M118 286L255 213L394 286L256 359L118 286Z" fill="#203B59" />
        <path d="M126 274L371 153L293 341L251 282L126 274Z" fill={`url(#${gradientId})`} />
        <path d="M251 282L371 153L293 341L251 282Z" fill="#2563EB" opacity="0.62" />
        <path
          d="M126 274L251 282L293 341"
          stroke="#A5F3FC"
          strokeWidth="8"
          strokeLinecap="round"
          strokeLinejoin="round"
          opacity="0.95"
        />
      </g>
      {/* 字母部分：原型里 font-size 17 / 650 / letter-spacing -0.02em（尺寸随 viewBox 缩放） */}
      <text
        x="36"
        y="22"
        fill="currentColor"
        style={{
          fontFamily: '"Albert Sans", "Noto Sans SC", sans-serif',
          fontSize: 17,
          fontWeight: 650,
          letterSpacing: '-0.02em',
        }}
      >
        Argus
      </text>
    </svg>
  )
}
