import { lazy } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useParams } from 'react-router-dom'
import { AppShell } from '@/components/app/AppShell'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardHeader } from '@/components/ui/card'
import { ToastProvider } from '@/components/ui/toast'

/* 路由级拆包（design/impl/web-and-delivery.md §6：每个 route lazyRouteComponent） */
const DashboardPage = lazy(() => import('@/pages/DashboardPage').then((m) => ({ default: m.DashboardPage })))
const RunsPage = lazy(() => import('@/pages/RunsPage').then((m) => ({ default: m.RunsPage })))
const RunDetailPage = lazy(() => import('@/pages/RunDetailPage').then((m) => ({ default: m.RunDetailPage })))
const SubmitPage = lazy(() => import('@/pages/SubmitPage').then((m) => ({ default: m.SubmitPage })))
const TestersPage = lazy(() => import('@/pages/TestersPage').then((m) => ({ default: m.TestersPage })))
const ClustersPage = lazy(() => import('@/pages/ClustersPage').then((m) => ({ default: m.ClustersPage })))

/** 换 Run 时整体重挂：详情页内部的选中节点/Attempt 状态一律重置。 */
function RunDetailRoute() {
  const { runId = '' } = useParams<{ runId: string }>()
  return <RunDetailPage key={runId} />
}

function NotFoundPage() {
  return (
    <Card>
      <CardHeader tone="warn" title="页面不存在" />
      <CardBody className="flex flex-col items-start gap-3">
        <p className="text-sm text-fg-2">这个地址没有对应的页面。</p>
        <Link to="/">
          <Button variant="primary" size="sm">
            回概览
          </Button>
        </Link>
      </CardBody>
    </Card>
  )
}

export function App() {
  return (
    <ToastProvider>
      <BrowserRouter>
        <Routes>
          <Route element={<AppShell />}>
            <Route index element={<DashboardPage />} />
            <Route path="runs" element={<RunsPage />} />
            <Route path="runs/:runId" element={<RunDetailRoute />} />
            <Route path="submit" element={<SubmitPage />} />
            <Route path="testers" element={<TestersPage />} />
            <Route path="clusters" element={<ClustersPage />} />
            {/* 旧链接兼容：Endpoints 页已改名为「集群与队列」 */}
            <Route path="endpoints" element={<Navigate to="/clusters" replace />} />
            <Route path="*" element={<NotFoundPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ToastProvider>
  )
}
