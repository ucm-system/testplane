package executor

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ucm-system/testplane/server/internal/ports"
)

// Pod 日志读取（D-59：Deploy 日志落盘的数据源）。
//
// 只读：`pods` 列表 + `pods/log` 流。平台**不**授予 `pods/exec`（execution §8）。
// 与 Tester 的 `Logs(handle)` 分开：这里要对 Genet namespace 里任意 Pod/容器抓日志，
// 还要能抓 previous（重启前的上一份容器日志）。

// ListPods 按 namespace 列出 Pod 及其容器状态（restarts/last_exit 供"补 previous 段"判定）。
func (e *Executor) ListPods(ctx context.Context, namespace string) ([]ports.PodInfo, error) {
	if namespace == "" {
		namespace = e.namespace
	}
	list, err := e.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: list pods %s: %w", namespace, err)
	}
	out := make([]ports.PodInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, podInfoOf(&list.Items[i]))
	}
	return out, nil
}

// StreamLogs 打开一个只读日志流（follow/timestamps/previous 由调用方决定）。
func (e *Executor) StreamLogs(ctx context.Context, namespace, pod, container string,
	opts ports.LogStreamOptions) (io.ReadCloser, error) {

	if namespace == "" {
		namespace = e.namespace
	}
	o := &corev1.PodLogOptions{
		Container:  opts.Container,
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
		Previous:   opts.Previous,
		TailLines:  opts.TailLines,
	}
	if opts.SinceTime != nil {
		t := metav1.NewTime(*opts.SinceTime)
		o.SinceTime = &t
	}
	rc, err := e.client.CoreV1().Pods(namespace).GetLogs(pod, o).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("executor: stream logs %s/%s[%s]: %w", namespace, pod, opts.Container, err)
	}
	return rc, nil
}

// 编译期断言：Executor 实现了可选的 PodLogSource（D-59）。
var _ ports.PodLogSource = (*Executor)(nil)

func podInfoOf(pod *corev1.Pod) ports.PodInfo {
	info := ports.PodInfo{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Phase:     string(pod.Status.Phase),
		Node:      pod.Spec.NodeName,
		CreatedAt: pod.CreationTimestamp.Time.UTC(),
	}
	for _, c := range pod.Status.ContainerStatuses {
		if c.Ready {
			info.Ready = true
		}
		ci := ports.ContainerInfo{
			Name:     c.Name,
			Restarts: c.RestartCount,
		}
		switch {
		case c.State.Running != nil:
			ci.StartedAt = c.State.Running.StartedAt.Time.UTC()
		case c.State.Terminated != nil:
			ci.StartedAt = c.State.Terminated.StartedAt.Time.UTC()
			ci.FinishedAt = c.State.Terminated.FinishedAt.Time.UTC()
			ci.LastExitCode = c.State.Terminated.ExitCode
			ci.LastExit = c.State.Terminated.Reason
		case c.State.Waiting != nil:
			ci.LastExit = c.State.Waiting.Reason
		}
		if c.LastTerminationState.Terminated != nil {
			ci.LastExit = c.LastTerminationState.Terminated.Reason
			if ci.LastExit == "" {
				ci.LastExit = fmt.Sprintf("exit %d", c.LastTerminationState.Terminated.ExitCode)
			}
		}
		info.Containers = append(info.Containers, ci)
	}
	return info
}
