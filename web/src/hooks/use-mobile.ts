import * as React from 'react'

const MOBILE_BREAKPOINT = 768

/**
 * `max-width: 767px` 时返回 true（与 Tailwind 的 `md` 断点对齐）。
 *
 * 取自 shadcn registry 的 `use-mobile`（`npx shadcn@latest view @shadcn/use-mobile`），
 * 只把 import 改成仓库里的 `React` 写法。
 */
export function useIsMobile(): boolean {
  const [isMobile, setIsMobile] = React.useState<boolean | undefined>(undefined)

  React.useEffect(() => {
    const mql = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`)
    const onChange = () => setIsMobile(window.innerWidth < MOBILE_BREAKPOINT)
    mql.addEventListener('change', onChange)
    onChange()
    return () => mql.removeEventListener('change', onChange)
  }, [])

  return !!isMobile
}
