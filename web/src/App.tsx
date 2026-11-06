import { lazy } from 'react'
import { BrowserRouter, Navigate, Route, Routes, useParams } from 'react-router-dom'
import { AppShell } from '@/components/app/AppShell'
import { ToastProvider } from '@/components/ui/toast'

/* 路由级拆包（design/impl/web-and-delivery.md §6：每个 route lazyRouteComponent） */
const DashboardPage = lazy(() => import('@/pages/DashboardPage').then((m) => ({ default: m.DashboardPage })))
const RunsPage = lazy(() => import('@/pages/RunsPage').then((m) => ({ default: m.RunsPage })))
const RunDetailPage = lazy(() => import('@/pages/RunDetailPage').then((m) => ({ default: m.RunDetailPage })))
const SubmitPage = lazy(() => import('@/pages/SubmitPage').then((m) => ({ default: m.SubmitPage })))
const TestersPage = lazy(() => import('@/pages/TestersPage').then((m) => ({ default: m.TestersPage })))
const ClustersPage = lazy(() => import('@/pages/ClustersPage').then((m) => ({ default: m.ClustersPage })))
const ReportPage = lazy(() => import('@/pages/ReportPage').then((m) => ({ default: m.ReportPage })))
const AdminPage = lazy(() => import('@/pages/AdminPage').then((m) => ({ default: m.AdminPage })))
const NotFoundPage = lazy(() => import('@/pages/NotFoundPage').then((m) => ({ default: m.NotFoundPage })))

/** 换 Run 时整体重挂：详情页内部的选中节点/Attempt 状态一律重置。 */
function RunDetailRoute() {
  const { runId = '' } = useParams<{ runId: string }>()
  return <RunDetailPage key={runId} />
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
            <Route path="runs/:runId/report" element={<ReportPage />} />
            <Route path="clusters" element={<ClustersPage />} />
            <Route path="admin" element={<AdminPage />} />
            {/* 旧链接兼容：Endpoints 页已改名为「集群与队列」 */}
            <Route path="endpoints" element={<Navigate to="/clusters" replace />} />
            {/*
              统一 404：`/404` 与所有未匹配路由（`*`）是**同一个组件**；
              `/runs/:id` 取到 404 时（见 RunDetailPage）也复用它，只换上下文文案。
            */}
            <Route path="404" element={<NotFoundPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ToastProvider>
  )
}
