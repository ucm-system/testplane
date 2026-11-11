package domain

import "testing"

func TestDecideTaskOutcome(t *testing.T) {
	okRunner := RunnerSummary{Seq: 3, Role: RoleTest, Status: RunnerSucceeded}
	failedRunner := RunnerSummary{Seq: 3, Role: RoleTest, Status: RunnerFailed, ErrorCode: CodeResultMissing}
	skippedRunner := RunnerSummary{Seq: 4, Role: RoleTest, Status: RunnerSkipped}
	cleanOK := RunnerSummary{Seq: 5, Role: RoleClean, Status: RunnerSucceeded}
	cleanFailed := RunnerSummary{Seq: 5, Role: RoleClean, Status: RunnerFailed, ErrorCode: CodeCleanupFailed}

	cases := []struct {
		name string
		in   TaskOutcomeInput
		want TaskStatus
	}{
		{
			name: "canceled 优先于一切",
			in: TaskOutcomeInput{Canceled: true, PlatformError: true,
				Runners: []RunnerSummary{{Seq: 1, Role: RoleDeploy, Status: RunnerFailed, ErrorCode: CodeDeployBlocked}}},
			want: TaskCanceled,
		},
		{
			name: "平台错误优先于部署阻塞",
			in: TaskOutcomeInput{PlatformError: true,
				Runners: []RunnerSummary{{Seq: 1, Role: RoleDeploy, Status: RunnerFailed, ErrorCode: CodeDeployBlocked}}},
			want: TaskFailed,
		},
		{
			// service-deploy 归类 DEPLOY_FAILED（Genet 调用失败）→ 平台侧失败，failed（C-5.8）。
			name: "DEPLOY_FAILED → failed",
			in: TaskOutcomeInput{Runners: []RunnerSummary{
				{Seq: 1, Role: RoleDeploy, Status: RunnerFailed, ErrorCode: CodeDeployFailed}}},
			want: TaskFailed,
		},
		{
			// blocked / 超部署窗口（DEPLOY_BLOCKED）→ service_unavailable（§7.5）。
			name: "DEPLOY_BLOCKED → service_unavailable",
			in: TaskOutcomeInput{Runners: []RunnerSummary{
				{Seq: 1, Role: RoleDeploy, Status: RunnerFailed, ErrorCode: CodeDeployBlocked}}},
			want: TaskServiceUnavailable,
		},
		{
			// 实例明确说可重试（ENDPOINT_UNREACHABLE + retryable:true）→ service_unavailable。
			name: "precheck 实例可重试 → service_unavailable",
			in: TaskOutcomeInput{Runners: []RunnerSummary{
				{Seq: 2, Role: RolePreCheck, Status: RunnerFailed, ErrorCode: CodeEndpointUnreachable,
					Retryable: true, HasRetryable: true},
				{Seq: 3, Role: RoleTest, Status: RunnerSkipped},
			}},
			want: TaskServiceUnavailable,
		},
		{
			// 码级兜底：实例没给 retryable，窗口内的码默认可重试。
			name: "precheck 无实例 retryable 时按码兜底 → service_unavailable",
			in: TaskOutcomeInput{Runners: []RunnerSummary{
				{Seq: 2, Role: RolePreCheck, Status: RunnerFailed, ErrorCode: CodeEndpointUnreachable},
				{Seq: 3, Role: RoleTest, Status: RunnerSkipped},
			}},
			want: TaskServiceUnavailable,
		},
		{
			// fail-401 场景：码在窗口内，但实例 retryable=false 必须覆盖码级默认。
			// v2：failed（v1 的 completed_with_errors 并入 failed，D-35）。
			name: "precheck 401（实例 retryable=false）→ failed",
			in: TaskOutcomeInput{Runners: []RunnerSummary{
				{Seq: 2, Role: RolePreCheck, Status: RunnerFailed, ErrorCode: CodeEndpointUnreachable,
					Retryable: false, HasRetryable: true},
			}},
			want: TaskFailed,
		},
		{
			// 实例说可重试，但码不在 C-5.8 第 4 条的窗口内 → 不算 service_unavailable。
			name: "precheck 码不在窗口内（即使实例 retryable=true）→ failed",
			in: TaskOutcomeInput{Runners: []RunnerSummary{
				{Seq: 2, Role: RolePreCheck, Status: RunnerFailed, ErrorCode: CodeExecutorUnavailable,
					Retryable: true, HasRetryable: true},
			}},
			want: TaskFailed,
		},
		{
			name: "全部成功（含 skipped）→ succeeded",
			in:   TaskOutcomeInput{Runners: []RunnerSummary{okRunner, skippedRunner, cleanOK}},
			want: TaskSucceeded,
		},
		{
			// clean 失败不影响终态（§7.5：只打 cleanup_failed）。
			name: "clean 失败不影响终态 → succeeded",
			in:   TaskOutcomeInput{Runners: []RunnerSummary{okRunner, cleanFailed}},
			want: TaskSucceeded,
		},
		{
			name: "单个 runner 失败 continue → failed",
			in:   TaskOutcomeInput{Runners: []RunnerSummary{okRunner, failedRunner}},
			want: TaskFailed,
		},
		{
			name: "全部 skipped → failed",
			in:   TaskOutcomeInput{Runners: []RunnerSummary{skippedRunner}},
			want: TaskFailed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideTaskOutcome(c.in); got != c.want {
				t.Fatalf("DecideTaskOutcome = %s, 期望 %s", got, c.want)
			}
		})
	}
}

func TestSkipReasonForTaskTerminal(t *testing.T) {
	cases := map[TaskStatus]string{
		TaskCanceled:           SkipCanceled,
		TaskCancelling:         SkipCanceled,
		TaskFailed:             SkipUpstreamAbort,
		TaskServiceUnavailable: SkipUpstreamAbort,
		TaskSucceeded:          SkipUpstreamAbort,
	}
	for to, want := range cases {
		if got := SkipReasonForTaskTerminal(to); got != want {
			t.Errorf("SkipReasonForTaskTerminal(%s) = %q, 期望 %q", to, got, want)
		}
	}
}
