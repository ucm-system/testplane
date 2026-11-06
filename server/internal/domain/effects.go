package domain

// Effects 是转移的附带动作（02 §3.5）：**只能是同事务 DB 写**。
// 外部动作（建/删 Job、Genet 调用、写报告文件）由 orchestrator 在事务之后执行，
// 失败以新命令回到状态机（如 Fail），不悬挂事务。

// EffectKind 是一种同事务副作用。
type EffectKind string

const (
	// EffectReleaseLeases 删除该 Task 的端点租约（Task 终态、preparing→failed/canceled）。
	EffectReleaseLeases EffectKind = "release_leases"
	// EffectSkipUnfinished 把同 Run 内未开始的 Task 批量置 skipped（Run 取消/失败收口）。
	EffectSkipUnfinished EffectKind = "skip_unfinished"
	// EffectMarkCancelRequested 给同 Run 内进行中的 Task 置 cancel_requested，供 executor 异步删 Job。
	EffectMarkCancelRequested EffectKind = "mark_cancel_requested"
	// EffectCloseActive 把同 Run 内仍进行中的 Task / Attempt 强制收口为 canceled，
	// 保证"Run 终态时所有 Task 终态"（C-5.8、C-3.4）。
	EffectCloseActive EffectKind = "close_active"
	// EffectScheduleRetry 重试：available_at = now + backoff_seconds。
	EffectScheduleRetry EffectKind = "schedule_retry"
	// EffectWriteReportIntent 插一条 report.pending 事件，供报告 writer 消费。
	EffectWriteReportIntent EffectKind = "write_report_intent"
	// EffectBackfillEndpoint 把 Run 的 endpoint_key 回填到该 Run 全部 Task（deploy ready / service）。
	EffectBackfillEndpoint EffectKind = "backfill_endpoint"
)

// Effects 返回 from→to 需要执行的同事务副作用；顺序即执行顺序。
//
// 与 02 §3.5 的对应：
//   - releaseLeases：Task 终态、preparing→failed/canceled；
//   - skipUnfinished / markCancelRequested：Run cancelling / canceled / failed 的收口；
//   - scheduleRetry：failed→queued；
//   - writeReportIntent：Task succeeded；
//   - backfillEndpoint：Run deploying→testing（C-5.3 的回填与转移同事务）。
func Effects(entity EntityType, from, to Status) []EffectKind {
	switch entity {
	case EntityTask:
		switch TaskStatus(to) {
		case TaskSucceeded:
			// 只释放租约。Task 级 report.json 在 MVP 不生成（Run 终态统一出报告，C-7.2）；
			// report.pending 也不在 07 §7 的 EventType 枚举里，所以不写这条事件。
			return []EffectKind{EffectReleaseLeases}
		case TaskFailed, TaskCanceled, TaskSkipped:
			return []EffectKind{EffectReleaseLeases}
		case TaskQueued:
			if TaskStatus(from) == TaskFailed {
				return []EffectKind{EffectScheduleRetry}
			}
		}
	case EntityRun:
		switch RunStatus(to) {
		case RunCancelling:
			// 取消意图：未开始的直接跳过，进行中的打标记等 executor 删 Job（C-6.1）。
			return []EffectKind{EffectSkipUnfinished, EffectMarkCancelRequested}
		case RunCanceled, RunFailed:
			// 平台侧终态前必须收口：未开始 → skipped，进行中 → canceled（C-5.8）。
			return []EffectKind{EffectSkipUnfinished, EffectCloseActive}
		case RunSucceeded, RunCompletedWithErrors, RunServiceUnavailable:
			// **任何终态**都必须收口（D-60）：R5（deploying → service_unavailable，部署阻塞/超窗）
			// 与 R7（cleanup → 三种终态）都可能有 Task 没跑过。之前只登记了 canceled/failed，
			// 于是 service_unavailable 的 Run 里残留 pending/queued Task（等待列表被灌满）。
			// 对 R7 的正常路径这两个 side-effect 是 no-op（进入 cleanup 前 Task 已全终态）。
			return []EffectKind{EffectSkipUnfinished, EffectCloseActive}
		case RunTesting:
			return []EffectKind{EffectBackfillEndpoint}
		}
	case EntityAttempt:
		if AttemptStatus(to).IsTerminal() {
			return []EffectKind{EffectReleaseLeases}
		}
	}
	return nil
}
