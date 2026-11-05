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
			name: "precheck 可重试失败 → service_unavailable",
			in: RunOutcomeInput{
				Precheck: &TaskSummary{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable},
				Tasks:    []TaskSummary{{Seq: 0, Status: TaskFailed, ErrorCode: CodeEndpointUnreachable}},
			},
			want: RunServiceUnavailable,
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
