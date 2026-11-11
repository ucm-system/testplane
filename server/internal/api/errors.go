package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/orchestrator"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 统一错误模型（07 §3、openapi Error）：**扁平**结构
//
//	{"code","message","detail","retryable","trace_id"}
//
// code 只用附录 D v2 枚举（domain.AllCodes 为同一集合）；HTTP 状态由 code 映射
// （httpStatusByCode）。未知错误一律 INTERNAL_ERROR + trace_id，绝不回传 pgx /
// client-go 原文（05 §6.1）。
type errorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Detail    map[string]any `json:"detail,omitempty"`
	Retryable bool           `json:"retryable"`
	TraceID   string         `json:"trace_id,omitempty"`
}

// httpStatusByCode 是附录 D v2 "HTTP" 列的机器版本（07 §3 的映射表）。
var httpStatusByCode = map[string]int{
	domain.CodeValidationError:         http.StatusBadRequest,
	domain.CodeUnauthenticated:         http.StatusUnauthorized,
	domain.CodeForbidden:               http.StatusForbidden,
	domain.CodeNotFound:                http.StatusNotFound,
	domain.CodeConflict:                http.StatusConflict,
	domain.CodeRunnerNotFound:          http.StatusBadRequest,
	domain.CodeRunnerVersionPinned:     http.StatusConflict,
	domain.CodeRunnerInvalid:           http.StatusBadRequest,
	domain.CodeExecutionProfileMissing: http.StatusBadRequest,
	domain.CodeTaskTooLarge:            http.StatusBadRequest,
	domain.CodeAttemptExhausted:        http.StatusConflict,
	domain.CodeResourceExhausted:       http.StatusTooManyRequests,
	domain.CodeArtifactQuotaExceeded:   http.StatusInsufficientStorage,
	domain.CodeExecutorUnavailable:     http.StatusServiceUnavailable,
	domain.CodeExecutorPodError:        http.StatusBadGateway,
	domain.CodeImagePullFailed:         http.StatusBadGateway,
	domain.CodeMountUnavailable:        http.StatusBadGateway,
	domain.CodeExportInvalid:           http.StatusBadRequest,
	domain.CodeEndpointUnreachable:     http.StatusBadGateway,
	domain.CodePrecheckFailed:          http.StatusBadGateway,
	domain.CodeRunnerTimeout:           http.StatusGatewayTimeout,
	domain.CodeRunnerNonzeroExit:       http.StatusInternalServerError,
	domain.CodeResultMissing:           http.StatusInternalServerError,
	domain.CodeResultInvalid:           http.StatusInternalServerError,
	domain.CodeCleanupFailed:           http.StatusInternalServerError,
	domain.CodeDeployFailed:            http.StatusBadGateway,
	domain.CodeDeployBlocked:           http.StatusBadGateway,
	domain.CodeCanceled:                http.StatusConflict,
	domain.CodeInternalError:           http.StatusInternalServerError,
}

// HTTPStatusForCode 返回错误码对应的 HTTP 状态；未知码按 500 处理。
func HTTPStatusForCode(code string) int {
	if s, ok := httpStatusByCode[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// writeFailure 按 domain.Failure 写错误响应。
func writeFailure(w http.ResponseWriter, r *http.Request, f domain.Failure) {
	writeErr(w, r, HTTPStatusForCode(f.Code), f.Code, f.Message, f.Detail, f.Retryable)
}

// fail 是写错误响应的统一入口。
func fail(w http.ResponseWriter, r *http.Request, status int, code, message string,
	detail map[string]any, retryable bool) {
	writeErr(w, r, status, code, message, detail, retryable)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, message string,
	detail map[string]any, retryable bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := errorBody{
		Code: code, Message: message, Detail: detail, Retryable: retryable,
		TraceID: RequestID(r.Context()),
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().Warn("写错误响应失败", "err", err, "request_id", RequestID(r.Context()))
	}
}

// failErr 把内部错误映射成附录 D 错误码（05 §6.1 的唯一映射点）。
func (s *Server) failErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		fail(w, r, http.StatusInternalServerError, domain.CodeInternalError, "未知错误", nil, false)

	case errors.Is(err, store.ErrBadCursor):
		fail(w, r, http.StatusBadRequest, domain.CodeValidationError, "游标无效", nil, false)

	case errors.Is(err, domain.ErrNotFound):
		fail(w, r, http.StatusNotFound, domain.CodeNotFound, "对象不存在", nil, false)

	case errors.Is(err, domain.ErrConflict):
		fail(w, r, http.StatusConflict, domain.CodeConflict, "对象状态已变化，请重试", nil, false)

	case errors.Is(err, domain.ErrPrecondition):
		// 守卫不通过：对调用方而言是"当前条件下不可执行"，按 409 暴露。
		fail(w, r, http.StatusConflict, domain.CodeConflict, "当前状态不满足操作前提", nil, false)

	case errors.Is(err, domain.ErrIllegalState):
		s.logger.Error("非法状态转移（编程错误）", "err", err, "request_id", RequestID(r.Context()))
		fail(w, r, http.StatusInternalServerError, domain.CodeInternalError, "内部状态机错误", nil, false)

	case errors.Is(err, domain.ErrTx):
		s.logger.Error("数据库事务失败", "err", err, "request_id", RequestID(r.Context()))
		fail(w, r, http.StatusInternalServerError, domain.CodeInternalError, "数据库暂不可用", nil, true)

	default:
		var ve *domain.ValidationError
		if errors.As(err, &ve) {
			writeFailure(w, r, ve.Failure())
			return
		}
		var fe *domain.FailureError
		if errors.As(err, &fe) {
			writeFailure(w, r, fe.Failure)
			return
		}
		var cre *orchestratorReplayConflict
		if errors.As(err, &cre) {
			fail(w, r, http.StatusConflict, domain.CodeConflict,
				"clean 重放前置条件不满足", cre.Detail, false)
			return
		}
		s.logger.Error("未归类错误", "err", err, "request_id", RequestID(r.Context()))
		fail(w, r, http.StatusInternalServerError, domain.CodeInternalError,
			"内部错误", map[string]any{"trace_id": RequestID(r.Context())}, false)
	}
}

// orchestratorReplayConflict 是 *orchestrator.ErrCleanReplayConflict 的包内别名
// （errors.As 目标类型必须是本包命名类型之外的同构指针 —— 这里直接用原类型）。
type orchestratorReplayConflict = orchestrator.ErrCleanReplayConflict

// JSON 输出助手 -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
