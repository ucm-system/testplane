import * as React from 'react'
import { PanelLeft } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Separator } from '@/components/ui/shadcn/separator'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/shadcn/sheet'
import { Skeleton } from '@/components/ui/shadcn/skeleton'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/shadcn/tooltip'
import { useIsMobile } from '@/hooks/use-mobile'
import { cn } from '@/lib/utils'

/**
 * Sidebar —— shadcn `@shadcn/sidebar` 的 new-york-v4 源码（`npx shadcn@latest view @shadcn/sidebar`）。
 *
 * 放进 `components/ui/shadcn/` 的**原因**（`--path` 的实测见交付说明）：
 * `npx shadcn add ... --path src/components/ui/shadcn` 虽然接受这个参数，但 dry-run 仍然把
 * 文件落到 alias 的 `src/components/ui/`，会覆盖我们自有的 button/input/skeleton；
 * 所以这里是**手抄 + 适配**，不是 CLI 产物。
 *
 * 与 registry 源码的差别（都是依赖/主题映射，不是视觉改写）：
 *  1. 不引 `radix-ui` 聚合包、`class-variance-authority`、`@radix-ui/react-slot`（仓库没装，也不许加依赖）；
 *     `asChild` 用 `React.cloneElement` 合并类名实现，cva 换成两个普通映射对象；
 *  2. 颜色类映射到仓库 tokens：`bg-sidebar`→`bg-surface-2`、`text-sidebar-foreground`→`text-fg`、
 *     `bg-sidebar-accent`→`bg-surface-2`、`border-sidebar-border`→`border-line`、`bg-background`→`bg-background`；
 *  3. `SidebarInset` 渲染成 `div`（registry 是 `<main>`）——外壳里真正的 landmark 是
 *     `<main id="main">`，避免一页两个 main；
 *  4. 折叠宽度：inset 变体下 `3rem + 1rem`（容器 64 → 内层刚好 48 的 icon rail），
 *     过渡沿用 registry 的 `transition-[left,right,width] duration-200 ease-linear`，没有自写时长；
 *  5. cookie 状态**会读回来**作为初始值（registry 只写不读），刷新后折叠状态不丢。
 */

const SIDEBAR_COOKIE_NAME = 'sidebar_state'
const SIDEBAR_COOKIE_MAX_AGE = 60 * 60 * 24 * 7
const SIDEBAR_WIDTH = '16rem'
const SIDEBAR_WIDTH_MOBILE = '18rem'
const SIDEBAR_WIDTH_ICON = '3rem'
const SIDEBAR_KEYBOARD_SHORTCUT = 'b'

function readSidebarCookie(): boolean | null {
  if (typeof document === 'undefined') return null
  const hit = document.cookie
    .split('; ')
    .find((entry) => entry.startsWith(`${SIDEBAR_COOKIE_NAME}=`))
  if (!hit) return null
  return hit.slice(SIDEBAR_COOKIE_NAME.length + 1) === 'true'
}

interface SidebarContextValue {
  state: 'expanded' | 'collapsed'
  open: boolean
  setOpen: (open: boolean | ((value: boolean) => boolean)) => void
  openMobile: boolean
  setOpenMobile: (open: boolean) => void
  isMobile: boolean
  toggleSidebar: () => void
}

const SidebarContext = React.createContext<SidebarContextValue | null>(null)

export function useSidebar(): SidebarContextValue {
  const context = React.useContext(SidebarContext)
  if (!context) throw new Error('useSidebar must be used within a SidebarProvider.')
  return context
}

export function SidebarProvider({
  defaultOpen = true,
  open: openProp,
  onOpenChange: setOpenProp,
  className,
  style,
  children,
  ...props
}: React.ComponentProps<'div'> & {
  defaultOpen?: boolean
  open?: boolean
  onOpenChange?: (open: boolean) => void
}) {
  const isMobile = useIsMobile()
  const [openMobile, setOpenMobile] = React.useState(false)
  // 组件自己写 cookie，也自己读回来（刷新后保持折叠/展开）。
  const [openState, setOpenState] = React.useState<boolean>(() => readSidebarCookie() ?? defaultOpen)
  const open = openProp ?? openState

  const setOpen = React.useCallback(
    (value: boolean | ((value: boolean) => boolean)) => {
      const next = typeof value === 'function' ? value(open) : value
      if (setOpenProp) setOpenProp(next)
      else setOpenState(next)
      document.cookie = `${SIDEBAR_COOKIE_NAME}=${next}; path=/; max-age=${SIDEBAR_COOKIE_MAX_AGE}; SameSite=Lax`
    },
    [open, setOpenProp],
  )

  const toggleSidebar = React.useCallback(() => {
    return isMobile ? setOpenMobile((value) => !value) : setOpen((value) => !value)
  }, [isMobile, setOpen])

  React.useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === SIDEBAR_KEYBOARD_SHORTCUT && (event.metaKey || event.ctrlKey)) {
        event.preventDefault()
        toggleSidebar()
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [toggleSidebar])

  const state: 'expanded' | 'collapsed' = open ? 'expanded' : 'collapsed'

  const contextValue = React.useMemo<SidebarContextValue>(
    () => ({ state, open, setOpen, isMobile, openMobile, setOpenMobile, toggleSidebar }),
    [state, open, setOpen, isMobile, openMobile, toggleSidebar],
  )

  return (
    <SidebarContext.Provider value={contextValue}>
      <TooltipProvider delayDuration={0}>
        <div
          data-slot="sidebar-wrapper"
          style={
            {
              '--sidebar-width': SIDEBAR_WIDTH,
              '--sidebar-width-icon': SIDEBAR_WIDTH_ICON,
              ...style,
            } as React.CSSProperties
          }
          className={cn(
            'group/sidebar-wrapper flex h-svh w-full overflow-hidden has-data-[variant=inset]:bg-surface-2',
            className,
          )}
          {...props}
        >
          {children}
        </div>
      </TooltipProvider>
    </SidebarContext.Provider>
  )
}

export function Sidebar({
  side = 'left',
  variant = 'sidebar',
  collapsible = 'offcanvas',
  className,
  children,
  ...props
}: React.ComponentProps<'div'> & {
  side?: 'left' | 'right'
  variant?: 'sidebar' | 'floating' | 'inset'
  collapsible?: 'offcanvas' | 'icon' | 'none'
}) {
  const { isMobile, state, openMobile, setOpenMobile } = useSidebar()

  if (collapsible === 'none') {
    return (
      <div
        data-slot="sidebar"
        className={cn('flex h-full w-(--sidebar-width) flex-col bg-surface text-fg', className)}
        {...props}
      >
        {children}
      </div>
    )
  }

  if (isMobile) {
    return (
      <Sheet open={openMobile} onOpenChange={setOpenMobile}>
        <SheetContent
          data-sidebar="sidebar"
          data-slot="sidebar"
          data-mobile="true"
          className="w-(--sidebar-width) max-w-none bg-surface p-0 text-fg [&>button]:hidden"
          style={{ '--sidebar-width': SIDEBAR_WIDTH_MOBILE } as React.CSSProperties}
          side={side}
        >
          <SheetHeader className="sr-only">
            <SheetTitle>导航</SheetTitle>
            <SheetDescription>移动端侧栏导航</SheetDescription>
          </SheetHeader>
          <div className="flex h-full w-full flex-col">{children}</div>
        </SheetContent>
      </Sheet>
    )
  }

  return (
    <div
      className="group peer hidden text-fg md:block"
      data-state={state}
      data-collapsible={state === 'collapsed' ? collapsible : ''}
      data-variant={variant}
      data-side={side}
      data-slot="sidebar"
    >
      {/* 桌面端占位：它撑开侧栏在布局里的宽度（fixed 的可见容器不占位） */}
      <div
        data-slot="sidebar-gap"
        className={cn(
          'relative w-(--sidebar-width) bg-transparent transition-[width] duration-200 ease-linear',
          'group-data-[collapsible=offcanvas]:w-0',
          'group-data-[side=right]:rotate-180',
          variant === 'floating' || variant === 'inset'
            ? 'group-data-[collapsible=icon]:w-[calc(var(--sidebar-width-icon)+1rem)]'
            : 'group-data-[collapsible=icon]:w-(--sidebar-width-icon)',
        )}
      />
      <div
        data-slot="sidebar-container"
        className={cn(
          'fixed inset-y-0 z-10 hidden h-svh w-(--sidebar-width) transition-[left,right,width] duration-200 ease-linear md:flex',
          side === 'left'
            ? 'left-0 group-data-[collapsible=offcanvas]:left-[calc(var(--sidebar-width)*-1)]'
            : 'right-0 group-data-[collapsible=offcanvas]:right-[calc(var(--sidebar-width)*-1)]',
          variant === 'floating' || variant === 'inset'
            ? 'p-2 group-data-[collapsible=icon]:w-[calc(var(--sidebar-width-icon)+1rem)]'
            : 'group-data-[collapsible=icon]:w-(--sidebar-width-icon) group-data-[side=left]:border-r group-data-[side=right]:border-l',
          className,
        )}
        {...props}
      >
        <div
          data-sidebar="sidebar"
          data-slot="sidebar-inner"
          className="flex h-full w-full flex-col bg-surface group-data-[variant=floating]:rounded-lg group-data-[variant=floating]:border group-data-[variant=floating]:border-line"
        >
          {children}
        </div>
      </div>
    </div>
  )
}

export function SidebarTrigger({
  className,
  onClick,
  ...props
}: React.ComponentProps<typeof Button>) {
  const { toggleSidebar } = useSidebar()

  return (
    <Button
      data-sidebar="trigger"
      data-slot="sidebar-trigger"
      variant="ghost"
      size="sm"
      aria-label="切换侧栏"
      className={cn('size-8 px-0', className)}
      onClick={(event) => {
        onClick?.(event)
        toggleSidebar()
      }}
      {...props}
    >
      <PanelLeft aria-hidden />
      <span className="sr-only">切换侧栏</span>
    </Button>
  )
}

export function SidebarRail({ className, ...props }: React.ComponentProps<'button'>) {
  const { toggleSidebar } = useSidebar()

  return (
    <button
      data-sidebar="rail"
      data-slot="sidebar-rail"
      aria-label="切换侧栏"
      tabIndex={-1}
      onClick={toggleSidebar}
      title="切换侧栏"
      className={cn(
        'absolute inset-y-0 z-20 hidden w-4 -translate-x-1/2 transition-all ease-linear group-data-[side=left]:-right-4 group-data-[side=right]:left-0 after:absolute after:inset-y-0 after:left-1/2 after:w-[2px] hover:after:bg-line sm:flex',
        'in-data-[side=left]:cursor-w-resize in-data-[side=right]:cursor-e-resize',
        '[[data-side=left][data-state=collapsed]_&]:cursor-e-resize [[data-side=right][data-state=collapsed]_&]:cursor-w-resize',
        className,
      )}
      {...props}
    />
  )
}

export function SidebarInset({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      data-slot="sidebar-inset"
      className={cn(
        'relative flex h-svh w-full min-h-0 min-w-0 flex-1 flex-col overflow-hidden bg-background',
        'md:peer-data-[variant=inset]:m-2 md:peer-data-[variant=inset]:ml-0 md:peer-data-[variant=inset]:rounded-xl md:peer-data-[variant=inset]:shadow-xs md:peer-data-[variant=inset]:peer-data-[state=collapsed]:ml-2',
        className,
      )}
      {...props}
    />
  )
}

export function SidebarInput({ className, ...props }: React.ComponentProps<typeof Input>) {
  return (
    <Input
      data-slot="sidebar-input"
      data-sidebar="input"
      className={cn('h-8 w-full bg-background shadow-none', className)}
      {...props}
    />
  )
}

export function SidebarHeader({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div data-slot="sidebar-header" data-sidebar="header" className={cn('flex flex-col gap-2 p-2', className)} {...props} />
  )
}

export function SidebarFooter({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div data-slot="sidebar-footer" data-sidebar="footer" className={cn('flex flex-col gap-2 p-2', className)} {...props} />
  )
}

export function SidebarSeparator({ className, ...props }: React.ComponentProps<typeof Separator>) {
  return <Separator data-slot="sidebar-separator" data-sidebar="separator" className={cn('mx-2 w-auto bg-line', className)} {...props} />
}

export function SidebarContent({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      data-slot="sidebar-content"
      data-sidebar="content"
      className={cn(
        'flex min-h-0 flex-1 flex-col gap-2 overflow-auto group-data-[collapsible=icon]:overflow-hidden',
        className,
      )}
      {...props}
    />
  )
}

export function SidebarGroup({ className, ...props }: React.ComponentProps<'div'>) {
  return <div data-slot="sidebar-group" data-sidebar="group" className={cn('relative flex w-full min-w-0 flex-col p-2', className)} {...props} />
}

export function SidebarGroupLabel({
  className,
  asChild = false,
  children,
  ...props
}: React.ComponentProps<'div'> & { asChild?: boolean }) {
  const classes = cn(
    'flex h-8 shrink-0 items-center rounded-md px-2 text-xs font-medium text-fg-3 outline-hidden transition-[margin,opacity] duration-200 ease-linear focus-visible:ring-2 focus-visible:ring-ring/50',
    'group-data-[collapsible=icon]:-mt-8 group-data-[collapsible=icon]:opacity-0',
    className,
  )
  if (asChild) {
    const child = React.Children.only(children) as React.ReactElement<Record<string, unknown>>
    return React.cloneElement(child, { 'data-slot': 'sidebar-group-label', 'data-sidebar': 'group-label', className: cn(classes, child.props.className as string) })
  }
  return (
    <div data-slot="sidebar-group-label" data-sidebar="group-label" className={classes} {...props}>
      {children}
    </div>
  )
}

export function SidebarGroupAction({
  className,
  asChild = false,
  children,
  ...props
}: React.ComponentProps<'button'> & { asChild?: boolean }) {
  const classes = cn(
    'absolute right-3 top-3.5 flex aspect-square w-5 items-center justify-center rounded-md p-0 text-fg outline-hidden transition-transform hover:bg-surface-2 focus-visible:ring-2 focus-visible:ring-ring/50 [&>svg]:size-4 [&>svg]:shrink-0',
    'after:absolute after:-inset-2 md:after:hidden',
    'group-data-[collapsible=icon]:hidden',
    className,
  )
  if (asChild) {
    const child = React.Children.only(children) as React.ReactElement<Record<string, unknown>>
    return React.cloneElement(child, { 'data-slot': 'sidebar-group-action', 'data-sidebar': 'group-action', className: cn(classes, child.props.className as string) })
  }
  return (
    <button type="button" data-slot="sidebar-group-action" data-sidebar="group-action" className={classes} {...props}>
      {children}
    </button>
  )
}

export function SidebarGroupContent({ className, ...props }: React.ComponentProps<'div'>) {
  return <div data-slot="sidebar-group-content" data-sidebar="group-content" className={cn('w-full text-sm', className)} {...props} />
}

export function SidebarMenu({ className, ...props }: React.ComponentProps<'ul'>) {
  return <ul data-slot="sidebar-menu" data-sidebar="menu" className={cn('flex w-full min-w-0 flex-col gap-1', className)} {...props} />
}

export function SidebarMenuItem({ className, ...props }: React.ComponentProps<'li'>) {
  return <li data-slot="sidebar-menu-item" data-sidebar="menu-item" className={cn('group/menu-item relative', className)} {...props} />
}

const MENU_BUTTON_VARIANTS = {
  default: 'hover:bg-surface-2 hover:text-fg',
  outline:
    'bg-background shadow-[0_0_0_1px_var(--color-line)] hover:bg-surface-2 hover:text-fg',
} as const

const MENU_BUTTON_SIZES = {
  default: 'h-8 text-sm',
  sm: 'h-7 text-xs',
  lg: 'h-12 text-sm group-data-[collapsible=icon]:p-0!',
} as const

export function SidebarMenuButton({
  asChild = false,
  isActive = false,
  variant = 'default',
  size = 'default',
  tooltip,
  className,
  children,
  ...props
}: React.ComponentProps<'button'> & {
  asChild?: boolean
  isActive?: boolean
  variant?: keyof typeof MENU_BUTTON_VARIANTS
  size?: keyof typeof MENU_BUTTON_SIZES
  tooltip?: string | React.ComponentProps<typeof TooltipContent>
}) {
  const { state, isMobile } = useSidebar()
  const classes = cn(
    'peer/menu-button flex w-full items-center gap-2 overflow-hidden rounded-md p-2 text-left text-sm outline-hidden transition-[width,height,padding] hover:bg-surface-2 hover:text-fg focus-visible:ring-2 focus-visible:ring-ring/50 active:bg-surface-2 disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50 data-[active=true]:bg-surface-2 data-[active=true]:font-medium data-[active=true]:text-fg [&>span:last-child]:truncate [&>svg]:size-4 [&>svg]:shrink-0',
    'group-data-[collapsible=icon]:size-8! group-data-[collapsible=icon]:p-2!',
    MENU_BUTTON_VARIANTS[variant],
    MENU_BUTTON_SIZES[size],
    className,
  )
  const shared = {
    'data-slot': 'sidebar-menu-button',
    'data-sidebar': 'menu-button',
    'data-size': size,
    'data-active': isActive,
    className: classes,
    ...props,
  } as React.HTMLAttributes<HTMLElement> & Record<string, unknown>

  const button = asChild ? (
    React.cloneElement(
      React.Children.only(children) as React.ReactElement<Record<string, unknown>>,
      { ...shared, className: cn(classes, (React.Children.only(children) as React.ReactElement<{ className?: string }>).props.className) },
    )
  ) : (
    <button {...(shared as React.ComponentProps<'button'>)}>{children}</button>
  )

  if (!tooltip) return button

  const tooltipProps: React.ComponentProps<typeof TooltipContent> =
    typeof tooltip === 'string' ? { children: tooltip } : tooltip

  return (
    <Tooltip>
      <TooltipTrigger asChild>{button}</TooltipTrigger>
      <TooltipContent side="right" align="center" hidden={state !== 'collapsed' || isMobile} {...tooltipProps} />
    </Tooltip>
  )
}

export function SidebarMenuAction({
  className,
  asChild = false,
  showOnHover = false,
  children,
  ...props
}: React.ComponentProps<'button'> & { asChild?: boolean; showOnHover?: boolean }) {
  const classes = cn(
    'absolute right-1 top-1.5 flex aspect-square w-5 items-center justify-center rounded-md p-0 text-fg outline-hidden transition-transform hover:bg-surface-2 focus-visible:ring-2 focus-visible:ring-ring/50 [&>svg]:size-4 [&>svg]:shrink-0',
    'after:absolute after:-inset-2 md:after:hidden',
    'peer-data-[size=sm]/menu-button:top-1 peer-data-[size=default]/menu-button:top-1.5 peer-data-[size=lg]/menu-button:top-2.5',
    'group-data-[collapsible=icon]:hidden',
    showOnHover &&
      'group-focus-within/menu-item:opacity-100 group-hover/menu-item:opacity-100 peer-data-[active=true]/menu-button:text-fg md:opacity-0',
    className,
  )
  if (asChild) {
    const child = React.Children.only(children) as React.ReactElement<Record<string, unknown>>
    return React.cloneElement(child, { 'data-slot': 'sidebar-menu-action', 'data-sidebar': 'menu-action', className: cn(classes, child.props.className as string) })
  }
  return (
    <button type="button" data-slot="sidebar-menu-action" data-sidebar="menu-action" className={classes} {...props}>
      {children}
    </button>
  )
}

export function SidebarMenuBadge({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      data-slot="sidebar-menu-badge"
      data-sidebar="menu-badge"
      className={cn(
        'pointer-events-none absolute right-1 flex h-5 min-w-5 select-none items-center justify-center rounded-md px-1 text-xs font-medium tabular-nums text-fg',
        'peer-data-[size=sm]/menu-button:top-1 peer-data-[size=default]/menu-button:top-1.5 peer-data-[size=lg]/menu-button:top-2.5',
        'group-data-[collapsible=icon]:hidden',
        className,
      )}
      {...props}
    />
  )
}

export function SidebarMenuSkeleton({
  className,
  showIcon = false,
  ...props
}: React.ComponentProps<'div'> & { showIcon?: boolean }) {
  const width = React.useMemo(() => `${Math.floor(Math.random() * 40) + 50}%`, [])
  return (
    <div
      data-slot="sidebar-menu-skeleton"
      data-sidebar="menu-skeleton"
      className={cn('flex h-8 items-center gap-2 rounded-md px-2', className)}
      {...props}
    >
      {showIcon ? <Skeleton className="size-4 rounded-md" data-sidebar="menu-skeleton-icon" /> : null}
      <Skeleton
        className="h-4 max-w-(--skeleton-width) flex-1"
        data-sidebar="menu-skeleton-text"
        style={{ '--skeleton-width': width } as React.CSSProperties}
      />
    </div>
  )
}

export function SidebarMenuSub({ className, ...props }: React.ComponentProps<'ul'>) {
  return (
    <ul
      data-slot="sidebar-menu-sub"
      data-sidebar="menu-sub"
      className={cn(
        'mx-3.5 flex min-w-0 translate-x-px flex-col gap-1 border-l border-line px-2.5 py-0.5',
        'group-data-[collapsible=icon]:hidden',
        className,
      )}
      {...props}
    />
  )
}

export function SidebarMenuSubItem({ className, ...props }: React.ComponentProps<'li'>) {
  return <li data-slot="sidebar-menu-sub-item" data-sidebar="menu-sub-item" className={cn('group/menu-sub-item relative', className)} {...props} />
}

export function SidebarMenuSubButton({
  asChild = false,
  size = 'md',
  isActive = false,
  className,
  children,
  ...props
}: React.ComponentProps<'a'> & { asChild?: boolean; size?: 'sm' | 'md'; isActive?: boolean }) {
  const classes = cn(
    'flex h-7 min-w-0 -translate-x-px items-center gap-2 overflow-hidden rounded-md px-2 text-fg outline-hidden hover:bg-surface-2 hover:text-fg focus-visible:ring-2 focus-visible:ring-ring/50 active:bg-surface-2 disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50 [&>span:last-child]:truncate [&>svg]:size-4 [&>svg]:shrink-0',
    'data-[active=true]:bg-surface-2 data-[active=true]:text-fg',
    size === 'sm' && 'text-xs',
    size === 'md' && 'text-sm',
    'group-data-[collapsible=icon]:hidden',
    className,
  )
  if (asChild) {
    const child = React.Children.only(children) as React.ReactElement<Record<string, unknown>>
    return React.cloneElement(child, { 'data-slot': 'sidebar-menu-sub-button', 'data-sidebar': 'menu-sub-button', 'data-active': isActive, className: cn(classes, child.props.className as string) })
  }
  return (
    <a data-slot="sidebar-menu-sub-button" data-sidebar="menu-sub-button" data-size={size} data-active={isActive} className={classes} {...props}>
      {children}
    </a>
  )
}
