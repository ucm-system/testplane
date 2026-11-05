package domain

import "testing"

func TestDecideRunOutcome(t *testing.T) {
	okTask := TaskSummary{Seq: 1, Status: TaskSucceeded}
	failedTask := TaskSummary{Seq: 1, Status: TaskFailed, ErrorCode: CodeResultMissing}
	skippedTask := TaskSummary{Seq: 2, Status: TaskSkipped}

	cases := []struct {
		name string
		in   RunOutcomeInput
		want RunStatus
	}{
		{
			name: "canceled 优先于一切",
			in:   RunOutcomeInput{Canceled: true, PlatformError: true, DeployBlocked: true},
			want: RunCanceled,
		},
		{
			name: "平台错误优先于部署阻塞",
			in:   RunOutcomeInput{PlatformError: true, DeployBlocked: true},
			want: RunFailed,
		},
		{
			name: "部署阻塞 → service_unavailable",
			in:   RunOutcomeInput{DeployBlocked: true, Tasks: []TaskSummary{okTask}},
			want: RunServiceUnavailable,
		},
		{
			// 实例明确说可重试（ENDPOINT_UNREACHABLE + retryable:true）→ service_unavailable。
			name: "precheck 实例可重试 → service_unavailable",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable,
					Retryable: true, HasRetryable: true},
				Tasks: []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable}},
			},
			want: RunServiceUnavailable,
		},
		{
			// 码级兜底：实例没给 retryable，窗口内的码默认可重试。
			name: "precheck 无实例 retryable 时按码兜底 → service_unavailable",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable},
				Tasks:    []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable}},
			},
			want: RunServiceUnavailable,
		},
		{
			// QA 的 fail-401 场景：码在窗口内，但实例 retryable=false 必须覆盖码级默认。
			name: "precheck 401（实例 retryable=false）→ completed_with_errors",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable,
					Retryable: false, HasRetryable: true},
				Tasks: []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable}},
			},
			want: RunCompletedWithErrors,
		},
		{
			// QA 的 error-in-200 场景：服务返回 200 但内容不对，实例标了不可重试。
			name: "precheck 200-包错误（实例 retryable=false）→ completed_with_errors",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodePrecheckFailed,
					Retryable: false, HasRetryable: true},
				Tasks: []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodePrecheckFailed}},
			},
			want: RunCompletedWithErrors,
		},
		{
			// 实例说可重试，但码不在 C-5.8 第 4 条的窗口内 → 不算 service_unavailable。
			name: "precheck 码不在窗口内（即使实例 retryable=true）→ completed_with_errors",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodeExecutorUnavailable,
					Retryable: true, HasRetryable: true},
				Tasks: []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodeExecutorUnavailable}},
			},
			want: RunCompletedWithErrors,
		},
		{
			name: "precheck 不可重试失败 → completed_with_errors",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodeTesterNonzeroExit, Retryable: false},
				Tasks:    []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodeTesterNonzeroExit}},
			},
			want: RunCompletedWithErrors,
		},
		{
			name: "全部成功（含 skipped）→ succeeded",
			in:   RunOutcomeInput{Tasks: []TaskSummary{okTask, skippedTask}},
			want: RunSucceeded,
		},
		{
			name: "单个 Task 失败 continue → completed_with_errors",
			in:   RunOutcomeInput{Tasks: []TaskSummary{okTask, failedTask}},
			want: RunCompletedWithErrors,
		},
		{
			name: "单个 Task failed 但不重试也算有错",
			in:   RunOutcomeInput{Tasks: []TaskSummary{failedTask}},
			want: RunCompletedWithErrors,
		},
		{
			name: "全部 skipped → completed_with_errors",
			in:   RunOutcomeInput{Tasks: []TaskSummary{skippedTask}},
			want: RunCompletedWithErrors,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideRunOutcome(c.in); got != c.want {
				t.Fatalf("DecideRunOutcome = %s, 期望 %s", got, c.want)
			}
		})
	}
}

func TestSkipReasonForRunTerminal(t *testing.T) {
	cases := map[RunStatus]string{
		RunCanceled:           SkipCanceled,
		RunFailed:             SkipPlatformError,
		RunServiceUnavailable: SkipServiceUnavailable,
		RunSucceeded:          SkipUpstreamAbort,
	}
	for to, want := range cases {
		if got := SkipReasonForRunTerminal(to); got != want {
			t.Errorf("SkipReasonForRunTerminal(%s) = %q, 期望 %q", to, got, want)
		}
	}
}
