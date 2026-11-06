package domain

import (
	"fmt"
	"sort"
)

// 错误码（ARCHITECTURE 附录 D 的枚举，唯一来源）。规则：
//   - 只有 retryable=true 的码允许出现在 tasks.retry_policy.retry_on 白名单；
//   - 新增码必须同步附录 D 与本文件，并在 API 映射表里给出 HTTP 状态。

const (
	CodeValidationError         = "VALIDATION_ERROR"
	CodeUnauthenticated         = "UNAUTHENTICATED"
	CodeForbidden               = "FORBIDDEN"
	CodeNotFound                = "NOT_FOUND"
	CodeConflict                = "CONFLICT"
	CodeTesterNotFound          = "TESTER_NOT_FOUND"
	CodeTesterVersionPinned     = "TESTER_VERSION_PINNED"
	CodeTesterInvalid           = "TESTER_INVALID"
	CodeExecutionProfileMissing = "EXECUTION_PROFILE_NOT_FOUND"
	CodeRunTooLarge             = "RUN_TOO_LARGE"
	CodeResourceExhausted       = "RESOURCE_EXHAUSTED"
	CodeArtifactQuotaExceeded   = "ARTIFACT_QUOTA_EXCEEDED"
	CodeExecutorUnavailable     = "EXECUTOR_UNAVAILABLE"
	// CodeExecutorPodError 是 Pod 侧异常终止（Evicted / OOMKilled / 节点驱逐等）：
	// 平台侧错误（附录 D：可重试=是），detail.k8s_reason 保留 K8s 原文原因。
	CodeExecutorPodError    = "EXECUTOR_POD_ERROR"
	CodeImagePullFailed     = "IMAGE_PULL_FAILED"
	CodeMountUnavailable    = "MOUNT_UNAVAILABLE"
	CodeEndpointUnreachable = "ENDPOINT_UNREACHABLE"
	CodePrecheckFailed      = "PRECHECK_FAILED"
	CodeTaskTimeout         = "TASK_TIMEOUT"
	CodeTesterNonzeroExit   = "TESTER_NONZERO_EXIT"
	// Tester 自报的语义化失败码（附录 D）：不与 TESTER_NONZERO_EXIT 混淆，
	// 平台据 code 就能区分"阈值与能力矛盾"和"根本找不到可行并发"。
	CodeSLAPrecheckFailed        = "SLA_PRECHECK_FAILED"
	CodeSLANoFeasibleConcurrency = "SLA_NO_FEASIBLE_CONCURRENCY"
	CodeResultMissing            = "RESULT_MISSING"
	CodeResultInvalid            = "RESULT_INVALID"
	CodeCleanupFailed            = "CLEANUP_FAILED"
	// 部署失败只折叠成两个码（附录 D，D-46/D-47）；细分走 Failure.Detail["reason"]。
	CodeDeployFailed  = "DEPLOY_FAILED"  // 502 不可重试：平台侧调 Genet 失败
	CodeDeployBlocked = "DEPLOY_BLOCKED" // 502 可重试：Genet blocked 或超窗口
	CodeCanceled      = "CANCELED"
	CodeInternalError = "INTERNAL_ERROR"

	// 下面几个只作为 detail.issues[].code 出现，不是附录 D 的顶层码：
	// 顶层统一 VALIDATION_ERROR，issues 里定位具体原因。
	IssueCodeRequired       = "REQUIRED"
	IssueCodeTypeMismatch   = "TYPE_MISMATCH"
	IssueCodeEnum           = "ENUM"
	IssueCodeArgsLex        = "ARGS_LEX_ERROR"
	IssueCodePublicParam    = "PUBLIC_PARAM_CONFLICT"
	IssueCodeArgsTooMany    = "ARGS_TOO_MANY"
	IssueCodeArgTooLarge    = "ARG_TOO_LARGE"
	IssueCodeUnknownField   = "UNKNOWN_FIELD"
	IssueCodeDeployService  = "DEPLOY_SERVICE_EXCLUSIVE"
	IssueCodeOnFailure      = "ON_FAILURE_WEAKENED"
	IssueCodeEnvMissing     = "ENV_MISSING"
	IssueCodeSecretMissing  = "SECRET_NOT_FOUND"
	IssueCodeSecretRequired = "SECRET_REQUIRED"
	IssueCodePrecheckCount  = "PRECHECK_TARGET_AMBIGUOUS"
	IssueCodeCleanupPolicy  = "CLEANUP_POLICY"
	IssueCodeTimeoutRange   = "TIMEOUT_OUT_OF_RANGE"
	IssueCodeNameConflict   = "NAME_CONFLICT"
	IssueCodeConflict       = "CONFLICT"
	IssueCodeUnsupported    = "UNSUPPORTED"
	IssueCodePublicRequired = "PUBLIC_PARAM_REQUIRED"
	// IssueCodeTooLong 用于"长度超过设计上限"（D-58：run.name / task.name / tests[].name ≤ 200 字符）。
	IssueCodeTooLong = "TOO_LONG"
)

// retryableByCode 是附录 D "可重试" 列的机器版本。
var retryableByCode = map[string]bool{
	CodeResourceExhausted:        true,
	CodeExecutorUnavailable:      true,
	CodeExecutorPodError:         true, // 附录 D：可重试=是（换节点/重调度后可能成功）
	CodeImagePullFailed:          true,
	CodeEndpointUnreachable:      true,
	CodePrecheckFailed:           true,
	CodeCleanupFailed:            true,
	CodeDeployFailed:             false, // 调用 Genet 失败属平台侧错误，重试大概率同样失败
	CodeDeployBlocked:            true,  // 排队/部署未就绪：等服务或扩容后可重试
	CodeInternalError:            true,  // HT 视情况：保守地标可通过实例字段覆盖
	CodeTaskTimeout:              false, // 按策略：由实例的 Failure.Retryable 覆盖
	CodeMountUnavailable:         false,
	CodeTesterNonzeroExit:        false,
	CodeSLAPrecheckFailed:        false, // 阈值与能力矛盾：重试同样失败，应改参数
	CodeSLANoFeasibleConcurrency: false, // 无可行并发：同上
	CodeResultMissing:            false,
	CodeResultInvalid:            false,
	CodeValidationError:          false,
	CodeTesterNotFound:           false,
	CodeExecutionProfileMissing:  false,
	CodeRunTooLarge:              false,
	CodeArtifactQuotaExceeded:    false,
	CodeConflict:                 false,
	CodeCanceled:                 false,
	CodeNotFound:                 false,
	CodeUnauthenticated:          false,
	CodeForbidden:                false,
	CodeTesterVersionPinned:      false,
	CodeTesterInvalid:            false,
}

// Retryable 返回码级默认是否可重试。实例级 Failure.Retryable 优先（01 UC-07）。
func Retryable(code string) bool { return retryableByCode[code] }

// AllCodes 返回附录 D 的全集（供 tools/contract 做"附录 D ↔ openapi ↔ codes.go"
// 三方集合相等校验；返回副本，调用方随便改）。
func AllCodes() []string {
	out := make([]string, 0, len(retryableByCode))
	for code := range retryableByCode {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// KnownCode 报告 code 是否是附录 D 的枚举值（用于"自由字符串但只允许枚举"的校验）。
func KnownCode(code string) bool {
	_, ok := retryableByCode[code]
	return ok
}

// NewFailure 按码级默认 retryable 构造 Failure。
func NewFailure(code, message string, detail map[string]any) Failure {
	return Failure{Code: code, Message: message, Detail: detail, Retryable: Retryable(code)}
}

// Failuref 是 NewFailure 的格式化版本。
func Failuref(code string, detail map[string]any, format string, args ...any) Failure {
	return NewFailure(code, fmt.Sprintf(format, args...), detail)
}

// FailureError 把 domain.Failure 包装成 error，供 adapter / store 在只需要 error
// 的签名里携带错误码（api 用 errors.As 取回并映射 HTTP 状态）。
type FailureError struct{ Failure Failure }

func (e *FailureError) Error() string {
	return e.Failure.Code + ": " + e.Failure.Message
}

// NewFailureError 构造一个可被 errors.As 取出的失败错误。
func NewFailureError(code, message string, detail map[string]any) *FailureError {
	return &FailureError{Failure: NewFailure(code, message, detail)}
}

// ValidationIssue 是提交校验的字段级问题（07 §3、C-8.3）。
// Path 用 JSON Pointer 风格（tests[2].args），供 UI 直接定位输入。
type ValidationIssue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationError 聚合一次提交校验的多个 issue。
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "validation failed"
	}
	return fmt.Sprintf("validation failed: %s (%s)", e.Issues[0].Path, e.Issues[0].Code)
}

// Add 追加一条 issue，返回自身便于链式构造。
func (e *ValidationError) Add(path, code, format string, args ...any) *ValidationError {
	e.Issues = append(e.Issues, ValidationIssue{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
	return e
}

// Empty 报告是否没有 issue。
func (e *ValidationError) Empty() bool { return e == nil || len(e.Issues) == 0 }

// Failure 把校验错误折叠成附录 D 的 Failure（VALIDATION_ERROR + detail.issues）。
func (e *ValidationError) Failure() Failure {
	issues := make([]any, 0, len(e.Issues))
	for _, is := range e.Issues {
		issues = append(issues, map[string]any{"path": is.Path, "code": is.Code, "message": is.Message})
	}
	return NewFailure(CodeValidationError, "提交内容未通过校验", map[string]any{"issues": issues})
}
