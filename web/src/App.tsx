import { lazy } from 'react'
import { BrowserRouter, Navigate, Route, Routes, useLocation, useParams } from 'react-router-dom'
import { AppShell } from '@/components/app/AppShell'
import { ToastProvider } from '@/components/ui/toast'

/* 路由级拆包（design/impl/web-and-delivery.md §6：每个 route lazyRouteComponent） */
const DashboardPage = lazy(() => import('@/pages/DashboardPage').then((m) => ({ default: m.DashboardPage })))
const TaskDetailPage = lazy(() => import('@/pages/TaskDetailPage').then((m) => ({ default: m.TaskDetailPage })))
const SubmitPage = lazy(() => import('@/pages/SubmitPage').then((m) => ({ default: m.SubmitPage })))
const RunnersPage = lazy(() => import('@/pages/RunnersPage').then((m) => ({ default: m.RunnersPage })))
const ClustersPage = lazy(() => import('@/pages/ClustersPage').then((m) => ({ default: m.ClustersPage })))
const ReportPage = lazy(() => import('@/pages/ReportPage').then((m) => ({ default: m.ReportPage })))
const AdminPage = lazy(() => import('@/pages/AdminPage').then((m) => ({ default: m.AdminPage })))
const NotFoundPage = lazy(() => import('@/pages/NotFoundPage').then((m) => ({ default: m.NotFoundPage })))
const DocsPage = lazy(() => import('@/pages/DocsPage').then((m) => ({ default: m.DocsPage })))
const ApiPage = lazy(() => import('@/pages/ApiPage').then((m) => ({ default: m.ApiPage })))

/** 换 Task 时整体重挂：详情页内部的选中节点/Attempt 状态一律重置。 */
function TaskDetailRoute() {
  const { taskId = '' } = useParams<{ taskId: string }>()
  return <TaskDetailPage key={taskId} />
}

/**
 * v1 兼容重定向（D-62 / D-73）：
 *  - `/runs?<query>` → `/?<query>`：列表页已并入概览，旧链接连同 query 一起转（旧书签不失效）；
 *  - `/runs/:id` → `/tasks/:id`；`/runs/:id/report` → `/tasks/:id/report`；
 *  - `/testers` → `/runners`（D-73 术语更名）。
 * 这里跑在 BrowserRouter（非 Data Router）下，所以用 `<Navigate>` 而不是 router 级 redirect。
 */
function RunsIndexRedirect() {
  const { search } = useLocation()
  return <Navigate to={{ pathname: '/', search }} replace />
}

function RunDetailRedirect() {
  const { runId = '' } = useParams<{ runId: string }>()
  const { pathname, search } = useLocation()
  const suffix = pathname.endsWith('/report') ? '/report' : ''
  return <Navigate to={`/tasks/${encodeURIComponent(runId)}${suffix}${search}`} replace />
}

function TestersRedirect() {
  const { search } = useLocation()
  return <Navigate to={{ pathname: '/runners', search }} replace />
}

export function App() {
  return (
    <ToastProvider>
      <BrowserRouter>
        <Routes>
          <Route element={<AppShell />}>
            <Route index element={<DashboardPage />} />
            {/*
              v2（D-73）：Task 是一等对象，`/tasks/$taskId` 是详情路由；
              概览仍承载全量 Task 表（D-62），`/tasks` 本身重定向回概览。
            */}
            <Route path="tasks" element={<RunsIndexRedirect />} />
            <Route path="tasks/:taskId" element={<TaskDetailRoute />} />
            <Route path="tasks/:taskId/report" element={<ReportPage />} />
            {/*
              D-62：概览与运行列表合并 —— `/runs` 保留成兼容重定向，**query 原样带过去**：
              /runs?status=failed&page=2 → /?status=failed&page=2；/runs/:id → /tasks/:id。
            */}
            <Route path="runs" element={<RunsIndexRedirect />} />
            <Route path="runs/:runId" element={<RunDetailRedirect />} />
            <Route path="runs/:runId/report" element={<RunDetailRedirect />} />
            <Route path="submit" element={<SubmitPage />} />
            <Route path="runners" element={<RunnersPage />} />
            {/* D-73：Tester → Runner 术语更名，旧链接重定向 */}
            <Route path="testers" element={<TestersRedirect />} />
            <Route path="clusters" element={<ClustersPage />} />
            <Route path="admin" element={<AdminPage />} />
            {/* 文档与 API 参考（侧栏底部的两条入口） */}
            <Route path="docs" element={<DocsPage />} />
            <Route path="api" element={<ApiPage />} />
            {/* 旧链接兼容：Endpoints 页已改名为「集群与队列」 */}
            <Route path="endpoints" element={<Navigate to="/clusters" replace />} />
            {/*
              统一 404：`/404` 与所有未匹配路由（`*`）是**同一个组件**；
              `/tasks/:id` 取到 404 时（见 TaskDetailPage）也复用它，只换上下文文案。
            */}
            <Route path="404" element={<NotFoundPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ToastProvider>
  )
}
