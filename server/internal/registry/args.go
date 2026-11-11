package registry

import (
	"fmt"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 提交期辅助（C-8.4 / C-8.5）。
//
// v2 裁决（D-66/D-69）：业务输入**不走 argv**——`BuildArgv` / `SplitArgs` /
// `InjectPublicParams` 随 argv 通道一起取消；输入原文按 Runner 的 input_format
// 校验（C-8.4）后落盘为输入文件（AG_PARAMS_FILE 注入路径），entry argv 恒空。
// 公共参数以 AG_SERVED_* 环境变量注入（C-8.5）。

// ValidateInput 是提交期输入校验的一步到位入口（C-8.4）：
// 按 Runner 声明的 input_format 校验输入原文；jsonl 额外按 parameters 做
// 类型 + required 存在性校验（D-67）。错误带行号 + 字段名。
// 返回的 issue 列表非空时，提交被拒（VALIDATION_ERROR，detail.issues）。
func ValidateInput(spec domain.RunnerSpec, raw string) []domain.ValidationIssue {
	params := make([]domain.Param, 0, len(spec.Parameters))
	params = append(params, spec.Parameters...)
	return domain.ValidateInput(spec.InputFormat, raw, params)
}

// RequiredConsumes 返回该 Runner 声明消费的上游 key（§5.12）。
// 组合期校验（orchestrator）据此检查每个 key 必须有上游导出、静态值或 secret 来源。
func RequiredConsumes(spec domain.RunnerSpec) []string {
	out := make([]string, len(spec.Consumes))
	copy(out, spec.Consumes)
	return out
}

// DeclaredExports 返回该 Runner 声明产出的下游 key（§5.12）。
// 组合期校验据此拒绝两个 Runner 声明同名导出。
func DeclaredExports(spec domain.RunnerSpec) []string {
	out := make([]string, len(spec.Exports))
	copy(out, spec.Exports)
	return out
}

// EndpointKey 计算 endpoint_key（C-8.5 第 5 条，仅外接模式租约用）。
// 解析失败时退回 raw:<sha256>，保证同样的字符串仍然互相排斥。
func EndpointKey(raw string) string { return domain.NormalizeEndpointKey(raw) }

// ValidateOnFailureMerge 执行 §7.3 的合并规则：Manifest 只能取**更严格者**。
// Runner 声明 abort_task 而 Manifest 写 continue 属配置冲突，返回错误（提交阶段拒绝）。
func ValidateOnFailureMerge(runnerDeclared, manifest string) error {
	if runnerDeclared == domain.OnFailureAbortTask && manifest == domain.OnFailureContinue {
		return fmt.Errorf("on_failure: runner declares abort_task but manifest says continue (config conflict, §7.3): %w",
			domain.ErrPrecondition)
	}
	switch manifest {
	case "", domain.OnFailureContinue, domain.OnFailureAbortTask:
		return nil
	}
	return fmt.Errorf("on_failure: %q is not continue|abort_task: %w", manifest, domain.ErrPrecondition)
}
