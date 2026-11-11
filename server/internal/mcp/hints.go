package mcp

import "strings"

// Hint 是「已知错误码 → Agent 下一步动作」的共享映射。
//
// 设计取舍：
//   - 只做**提示**，不改变控制面的错误语义；retryable 是码级默认值，
//     实际是否可重试仍以错误信封里的 retryable 为准（实例级优先，07 §3）；
//   - next_action 是一句可执行的命令/动作，hint 是解释与定位信息；
//   - CLI 与 MCP 共用这一份映射（CLI 已 import 本包），避免两个入口各说一套。
//
// 新增错误码时同步改这里与 design/impl/agent-integration.md §6。
type Hint struct {
	Code       string `json:"code"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action"`
	Hint       string `json:"hint,omitempty"`
}

var errorHints = map[string]Hint{
	"VALIDATION_ERROR": {
		Code: "VALIDATION_ERROR", Retryable: false,
		NextAction: "按 detail.issues[] 修正字段后重新提交；不要重试相同 Manifest",
		Hint:       "每个 issue 带 path（如 runners[2].input，jsonl 带 line 行号）、code、message，可直接定位",
	},
	"UNAUTHENTICATED": {
		Code: "UNAUTHENTICATED", Retryable: false,
		NextAction: "重新获取 token（$AG_TOKEN / --token）；dev 模式可先用 dev",
		Hint:       "Bearer token 缺失或过期；控制面不提供匿名端点（/healthz、/readyz 除外）",
	},
	"FORBIDDEN": {
		Code: "FORBIDDEN", Retryable: false,
		NextAction: "确认当前角色：viewer 只读，operator 才能提交/取消，admin 才能维护",
		Hint:       "403 由服务端统一判定；角色来自身份源，不要在客户端推断",
	},
	"NOT_FOUND": {
		Code: "NOT_FOUND", Retryable: false,
		NextAction: "核对 ID 与资源是否存在；不确定时先 argus_list_artifacts 或查 /queue-status",
		Hint:       "ID 是不透明字符串，不要解析或拼接",
	},
	"CONFLICT": {
		Code: "CONFLICT", Retryable: true,
		NextAction: "先 wait 到终态再重试；不要并发改同一个 Task 的取消/重试",
		Hint:       "乐观锁或状态前置条件不满足；以先落库状态为准（07 §8）",
	},
	"RUNNER_NOT_FOUND": {
		Code: "RUNNER_NOT_FOUND", Retryable: false,
		NextAction: "用 argus_list_runners 查看目录，修正 runner 名称或 version",
		Hint:       "Runner 来自注册表自发现，运行时不注册（K-3）",
	},
	"RUNNER_VERSION_PINNED": {
		Code: "RUNNER_VERSION_PINNED", Retryable: false,
		NextAction: "版本不可覆盖：发布新 version 后改 Manifest 引用，别复用旧版本号",
		Hint:       "同 name+version 不同内容会被拒绝（§5.7）",
	},
	"RUNNER_INVALID": {
		Code: "RUNNER_INVALID", Retryable: false,
		NextAction: "修 runner.yaml / entry；修好后等注册表重扫",
		Hint:       "Runner 定义未通过注册校验（附录 C v2）",
	},
	"EXECUTION_PROFILE_NOT_FOUND": {
		Code: "EXECUTION_PROFILE_NOT_FOUND", Retryable: false,
		NextAction: "用 argus_list_profiles 看已登记 Profile，改 runners[].execution.profile",
		Hint:       "Profile 必须已登记；引导期例外已移除",
	},
	"TASK_TOO_LARGE": {
		Code: "TASK_TOO_LARGE", Retryable: false,
		NextAction: "拆分 Task：单 Task runner 数 ≤200（含注入的 service-deploy / precheck / service-clean）",
		Hint:       "先减少 runners[] 或拆成多个 Task",
	},
	"ATTEMPT_EXHAUSTED": {
		Code: "ATTEMPT_EXHAUSTED", Retryable: false,
		NextAction: "attempt 已达上限（默认 3，§12.7）；换新 Task 或修正输入",
		Hint:       "v2 新增（D-73）；runner 级没有重试（D-69）",
	},
	"EXPORT_INVALID": {
		Code: "EXPORT_INVALID", Retryable: false,
		NextAction: "修 Runner 的 exports.env（每行 KEY=VALUE，值不含换行）",
		Hint:       "v2 新增（D-71）；词法不合格整体拒绝收割",
	},
	"RESOURCE_EXHAUSTED": {
		Code: "RESOURCE_EXHAUSTED", Retryable: true,
		NextAction: "指数退避后重试；或降低提交频率、等并发槽位释放",
		Hint:       "限流/配额打满（429）；等 1/2/4/8s 再试",
	},
	"ARTIFACT_QUOTA_EXCEEDED": {
		Code: "ARTIFACT_QUOTA_EXCEEDED", Retryable: false,
		NextAction: "清理旧 Task 产物（admin）或减少产出后重新提交",
		Hint:       "产物配额是平台级限制，与 Runner 无关",
	},
	"EXECUTOR_UNAVAILABLE": {
		Code: "EXECUTOR_UNAVAILABLE", Retryable: true,
		NextAction: "等 30–60s 重试 wait 或重新提交；不要改 Manifest",
		Hint:       "执行器/Job 提交暂不可用；控制面自身仍可读",
	},
	"EXECUTOR_POD_ERROR": {
		Code: "EXECUTOR_POD_ERROR", Retryable: false,
		NextAction: "看 k8s_reason（OOM/Evicted/NodeLost），调 resources 后重新提交",
		Hint:       "Pod 级异常不是测试失败；错误 detail 里有 image_digest 与 pod 信息",
	},
	"IMAGE_PULL_FAILED": {
		Code: "IMAGE_PULL_FAILED", Retryable: false,
		NextAction: "核对镜像名/tag 与 registry 凭据，确认节点能拉取后再提交",
		Hint:       "看 Attempt 的 k8s_reason 与事件；镜像 tag 可变，必要时改 digest",
	},
	"MOUNT_UNAVAILABLE": {
		Code: "MOUNT_UNAVAILABLE", Retryable: false,
		NextAction: "对照 profile.mounts 与 Runner 的 mounts 声明，补齐挂载能力后再提交",
		Hint:       "Profile 供给缺失，不是 Runner 参数问题",
	},
	"ENDPOINT_UNREACHABLE": {
		Code: "ENDPOINT_UNREACHABLE", Retryable: true,
		NextAction: "服务未就绪：等 10–30s 重试，或看 GET /tasks/{id}/deploy 的 phase 与水位",
		Hint:       "连接/健康检查失败；先确认端口、模型名与鉴权，再看 precheck 日志",
	},
	"PRECHECK_FAILED": {
		Code: "PRECHECK_FAILED", Retryable: false,
		NextAction: "检查被测服务的健康检查/端口/模型名；看 argus_get_logs 的 precheck runner",
		Hint:       "pre_check 是管道第一个测试类 runner（串行）；retryable 以实例字段为准",
	},
	"RUNNER_TIMEOUT": {
		Code: "RUNNER_TIMEOUT", Retryable: false,
		NextAction: "调大 runners[].timeout_seconds / AG_RUNNER_TIMEOUT 后整 Task retry",
		Hint:       "超时是终态；先确认 Runner 没有卡在等待外部资源",
	},
	"RUNNER_NONZERO_EXIT": {
		Code: "RUNNER_NONZERO_EXIT", Retryable: false,
		NextAction: "看 Runner 日志与 result.json 的 error（若有）；非 0 退出通常是测试失败",
		Hint:       "退出码与 stderr 最有用；重试入口只有整 Task retry（D-69）",
	},
	"RESULT_MISSING": {
		Code: "RESULT_MISSING", Retryable: false,
		NextAction: "看日志：runner 退出 0 但三选一产物全缺（D-74）",
		Hint:       "应产出 result.json/html/csv 至少其一",
	},
	"RESULT_INVALID": {
		Code: "RESULT_INVALID", Retryable: false,
		NextAction: "对照契约修 Runner 的 result.json（仅当 result.json 产出但不合法，D-74）",
		Hint:       "html/csv 平台不解析；字段不限制",
	},
	"CLEANUP_FAILED": {
		Code: "CLEANUP_FAILED", Retryable: true,
		NextAction: "看 service-clean 日志；可用 argus clean --task 重放（§9.3）",
		Hint:       "不改变 Task 终态，只记 cleanup_failed（C-5.7）",
	},
	"CANCELED": {
		Code: "CANCELED", Retryable: false,
		NextAction: "需要重跑就重新 submit（不做幂等，D-37）；取消是终态",
		Hint:       "重复取消返回 200 同状态（07 §8）",
	},
	"INTERNAL_ERROR": {
		Code: "INTERNAL_ERROR", Retryable: true,
		NextAction: "带 trace_id 报平台；可用指数退避重试一次",
		Hint:       "trace_id 与响应头 X-Request-Id 同源，是唯一能对上服务端日志的字段",
	},
	"DEPLOY_FAILED": {
		Code: "DEPLOY_FAILED", Retryable: false,
		NextAction: "service-deploy 调 Genet 失败，不要直接重试同样参数；需要重试走 argus retry（fresh deploy）",
		Hint:       "502；看 /tasks/{id}/deploy 的 failure.detail.reason",
	},
	"DEPLOY_BLOCKED": {
		Code: "DEPLOY_BLOCKED", Retryable: true,
		NextAction: "看 /tasks/{id}/deploy 的 phase 与水位（或 /queue-status volcano 块），等队列前进；需要重试走 argus retry",
		Hint:       "reason 原文在 deploy-state 的 failure.detail.reason；只有水位没有位次（D-46）",
	},
	// SERVICE_UNAVAILABLE 是 Task 终态（不是 ErrorCode），但 wait/status 需要同一份提示。
	"SERVICE_UNAVAILABLE": {
		Code: "SERVICE_UNAVAILABLE", Retryable: true,
		NextAction: "服务未就绪：等 10–30s 重新 submit；或改用 service.url 直连已有服务",
		Hint:       "Deploy 阶段收口为 service_unavailable；与 ENDPOINT_UNREACHABLE 同类",
	},
	// UNREACHABLE 是 CLI/MCP 的合成错误（传输层失败，控制面没返回信封）。
	"UNREACHABLE": {
		Code: "UNREACHABLE", Retryable: true,
		NextAction: "检查 --base-url / 控制面是否启动 / 网络；稍后重试",
		Hint:       "这不是控制面返回的业务错误；请求没有到达服务端",
	},
}

// HintForError 返回某个错误码的共享提示；未知码返回 false（调用方只输出原始信封）。
func HintForError(code string) (Hint, bool) {
	h, ok := errorHints[strings.ToUpper(strings.TrimSpace(code))]
	return h, ok
}

// HintForTaskStatus 给没有 error 的终态（canceled / service_unavailable）补提示。
func HintForTaskStatus(status string) (Hint, bool) {
	switch status {
	case "canceled":
		return errorHints["CANCELED"], true
	case "service_unavailable":
		return errorHints["SERVICE_UNAVAILABLE"], true
	default:
		return Hint{}, false
	}
}
