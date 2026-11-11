package domain

// Effects 是转移的附带动作（02 §3.5）：**只能是同事务 DB 写**。
// 外部动作（建/删 Job、写报告文件）由 orchestrator 在事务之后执行，
// 失败以新命令回到状态机（如 Fail），不悬挂事务。
//
// v2 取消的 effect：scheduleRetry（runner 级重试取消，D-69）；
// backfillEndpoint（公共参数改走 exports 注入通道，D-66/D-71）。

// EffectKind 是一种同事务副作用。
type EffectKind string

const (
	// EffectReleaseLeases 删除该 runner 执行记录的端点租约（runner 终态；
	// 仅外接模式有租约，部署模式为 no-op）。
	EffectReleaseLeases EffectKind = "release_leases"
	// EffectSkipUnfinished 把同 Task 内未开始的 runner 批量置 skipped
	// （取消/平台失败收口；传播到 clean 为止——clean 永不被 SKIPPED）。
	EffectSkipUnfinished EffectKind = "skip_unfinished"
	// EffectMarkCancelRequested 给进行中的 runner 置 cancel_requested，
	// 供 executor 异步删 Job。
	EffectMarkCancelRequested EffectKind = "mark_cancel_requested"
	// EffectCloseActive 把同 Task 内仍进行中的 runner 强制收口为 canceled，
	// 保证"Task 终态时所有 runner 终态"（02 §5）。
	EffectCloseActive EffectKind = "close_active"
	// EffectWriteReportIntent 插一条 report.written 前置事件，供报告 writer 消费。
	EffectWriteReportIntent EffectKind = "write_report_intent"
)

// Effects 返回 from→to 需要执行的同事务副作用；顺序即执行顺序。
//
// 与 02 §3.5 的对应：
//   - releaseLeases：runner 终态；
//   - skipUnfinished / markCancelRequested：Task cancelling 的收口；
//   - closeActive：Task canceled / failed（平台侧）终态前强制收口；
//   - writeReportIntent：Task 落终态。
func Effects(entity EntityType, from, to Status) []EffectKind {
	switch entity {
	case EntityRunnerRun:
		if RunnerStatus(to).IsTerminal() {
			return []EffectKind{EffectReleaseLeases}
		}
	case EntityTask:
		switch TaskStatus(to) {
		case TaskCancelling:
			// 取消意图：未开始的直接跳过（clean 除外），进行中的打标记等 executor 删 Job（C-6.1）。
			return []EffectKind{EffectSkipUnfinished, EffectMarkCancelRequested}
		case TaskCanceled:
			// clean 已完成、落终态前收口残余（正常路径下此 effect 是 no-op）。
			return []EffectKind{EffectCloseActive}
		case TaskFailed:
			// 平台侧失败：未开始 → skipped，进行中 → canceled（02 §5）。
			return []EffectKind{EffectSkipUnfinished, EffectCloseActive}
		case TaskSucceeded, TaskServiceUnavailable:
			// **任何终态**都必须收口（02 §5）；对正常路径是 no-op（进入终态前管道已全终态）。
			return []EffectKind{EffectSkipUnfinished, EffectCloseActive, EffectWriteReportIntent}
		}
	}
	return nil
}
