-- 一次性数据修复（D-60）：把"Run 已终态但从没被收口"的 Task 收尾。
--
-- 背景：`domain.Effects` 在 Run → service_unavailable（R5 部署阻塞/超窗）时漏了
-- `EffectSkipUnfinished`，导致这类 Run 里残留 pending/queued（以及极端情况下的
-- preparing/running）Task；它们永远不会再被调度，却长期挂在 `/queue-status` 的
-- "等待探活"列表与概览 KPI 的"排队中 Task"里。
--
-- 本修复与 `effectSkipUnfinished` / `effectCloseActive` 的语义保持一致：
--   pending/queued      → skipped （status_reason 取 Run 终态对应的合法 skip 原因）
--   preparing/running   → canceled（同上，并置 cancel_requested）
--   waiting_reason      → NULL   （不再伪装成"在等队列/在等探活"）
--
-- 幂等：只动"终态 Run 下仍非终态"的 Task，重复执行不会产生额外影响。

UPDATE tasks t
   SET status = CASE
                  WHEN t.status IN ('pending', 'queued') THEN 'skipped'
                  ELSE 'canceled'
                END,
       status_reason = CASE r.status
                         WHEN 'canceled'           THEN 'canceled'
                         WHEN 'failed'             THEN 'platform_error'
                         WHEN 'service_unavailable' THEN 'service_unavailable'
                         ELSE 'upstream_abort'
                       END,
       cancel_requested = CASE WHEN t.status IN ('preparing', 'running') THEN true ELSE t.cancel_requested END,
       waiting_reason = NULL,
       updated_at = now()
  FROM runs r
 WHERE r.id = t.run_id
   AND r.status IN ('succeeded', 'completed_with_errors', 'failed', 'canceled', 'service_unavailable')
   AND t.status IN ('pending', 'queued', 'preparing', 'running');
