import { Navigate, useParams } from 'react-router-dom'

/**
 * 报告页重定向（D-74 / 用户 2026-11-11）：「报告」页签与独立报告页已取消 ——
 * report.json 只是平台产物之一，不再强制依赖；旧链接 `/tasks/:id/report` 回详情页
 * （终态默认「结果」页签，结论 + 三选一渲染 + 产物下载都在那里）。
 */
export function ReportRedirect() {
  const { taskId = '' } = useParams<{ taskId: string }>()
  return <Navigate to={`/tasks/${encodeURIComponent(taskId)}`} replace />
}
