package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// Writer 实现 ports.ReportWriter：把一个 Task 的库内终态写成文件产物（C-7.2）。
//
// 文件布局（§8.1，路径相对 <storage-root>/<task_id>/）：
//
//	report.json   report.md   metadata.json   events.jsonl
//
// 幂等：同名文件覆盖写；events.jsonl 按 seq 全量重写（库是权威）。
type Writer struct {
	// Store 是唯一数据来源（库优先，不依赖"文件先于状态存在"）。
	Store *store.Store
	// Root 是 <storage-root>（产物按 <task_id>/attempts/<n>/... 组织，§6.8）。
	Root string
	// MaxReportBytes 是单 Task 报告上限（默认 20 MiB，超限写摘要 + report.full.json，§12.7）。
	MaxReportBytes int64
	Logger         *slog.Logger
	// Now 便于测试注入。
	Now func() time.Time
}

var _ interface {
	WriteTaskReport(ctx context.Context, taskID string) (path string, err error)
} = (*Writer)(nil)

// WriteTaskReport 生成 report.json / report.md / metadata.json / events.jsonl 并重建产物索引。
// 返回 report.json 的绝对路径。
func (w *Writer) WriteTaskReport(ctx context.Context, taskID string) (string, error) {
	lg := w.Logger
	if lg == nil {
		lg = slog.Default()
	}
	if w.Store == nil {
		return "", fmt.Errorf("report: writer 缺少 store")
	}
	bundle, err := w.Store.ReportBundle(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("report: 读 Task %s: %w", taskID, err)
	}

	dir := filepath.Join(w.Root, taskID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("report: 建目录 %s: %w", dir, err)
	}

	// 1. events.jsonl：按 seq 全量重写（库是权威，重启可重建，C-7.4）
	if err := writeEventsJSONL(filepath.Join(dir, "events.jsonl"), bundle.Events); err != nil {
		return "", err
	}

	// 2. report.json（超限时写摘要 + report.full.json，§12.7）
	payload := buildReportJSON(bundle)
	full, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("report: marshal report.json: %w", err)
	}
	maxBytes := w.MaxReportBytes
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	reportPath := filepath.Join(dir, "report.json")
	fullPath := filepath.Join(dir, "report.full.json")
	if int64(len(full)) > maxBytes {
		if err := os.WriteFile(fullPath, full, 0o640); err != nil {
			return "", fmt.Errorf("report: 写 report.full.json: %w", err)
		}
		summary := map[string]any{
			"task_id":        payload["task_id"],
			"attempt_no":     payload["attempt_no"],
			"status":         payload["status"],
			"counts":         payload["counts"],
			"truncated":      true,
			"report_ref":     "report.full.json",
			"schema_version": SchemaVersion2,
		}
		summaryJSON, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return "", fmt.Errorf("report: marshal report summary: %w", err)
		}
		if err := os.WriteFile(reportPath, summaryJSON, 0o640); err != nil {
			return "", fmt.Errorf("report: 写 report.json: %w", err)
		}
	} else if err := os.WriteFile(reportPath, full, 0o640); err != nil {
		return "", fmt.Errorf("report: 写 report.json: %w", err)
	}

	// 3. report.md（C-7.3 模板：状态/耗时/管道表/失败清单/链接）
	md := buildReportMarkdown(bundle, w.now())
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(md), 0o640); err != nil {
		return "", fmt.Errorf("report: 写 report.md: %w", err)
	}

	// 4. metadata.json：裁决快照（版本、镜像引用 + 实际 digest、Profile、Manifest 原文）
	meta, err := json.MarshalIndent(buildMetadata(bundle), "", "  ")
	if err != nil {
		return "", fmt.Errorf("report: marshal metadata.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), meta, 0o640); err != nil {
		return "", fmt.Errorf("report: 写 metadata.json: %w", err)
	}

	// 5. 产物索引重建（派生数据，可重建；扫描该 attempt 目录）
	attemptNo := 0
	if bundle.Attempt != nil {
		attemptNo = bundle.Attempt.AttemptNo
	}
	if attemptNo > 0 {
		attemptDir := filepath.Join(dir, "attempts", fmt.Sprintf("%d", attemptNo))
		if err := w.indexArtifacts(ctx, taskID, attemptDir); err != nil {
			lg.Warn("report: 产物索引重建失败（可重建，不回滚终态）", "task_id", taskID, "err", err)
		}
	}
	lg.Info("report: 生成完成", "task_id", taskID)
	return reportPath, nil
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// ---- report.json（§8.4 汇总结构：只做组织与索引，不做指标数学聚合）-----------

func buildReportJSON(b *store.TaskReportBundle) map[string]any {
	runners := make([]map[string]any, 0, len(b.Runners))
	counts := map[string]int{"total": 0, "succeeded": 0, "failed": 0, "skipped": 0, "canceled": 0}
	failures := make([]map[string]any, 0, 2)
	cleanupFailed := false
	for _, rr := range b.Runners {
		item := map[string]any{
			"seq":     rr.Seq,
			"name":    rr.Name,
			"role":    rr.Role,
			"status":  rr.Status,
			"attempt": rr.AttemptNo,
		}
		if rr.ExportsRef != nil {
			item["exports_ref"] = *rr.ExportsRef
		}
		if rr.ResultRef != nil {
			item["result_ref"] = *rr.ResultRef
		}
		if rr.StartedAt != nil && rr.FinishedAt != nil {
			item["duration_ms"] = rr.FinishedAt.Sub(*rr.StartedAt).Milliseconds()
		}
		runners = append(runners, item)

		switch domain.RunnerStatus(rr.Status) {
		case domain.RunnerSucceeded:
			counts["succeeded"]++
		case domain.RunnerFailed:
			counts["failed"]++
		case domain.RunnerSkipped:
			counts["skipped"]++
		case domain.RunnerCanceled:
			counts["canceled"]++
		}
		if rr.Role == domain.RoleClean && rr.Status == string(domain.RunnerFailed) {
			cleanupFailed = true
		}
		if rr.ErrorCode != nil && (rr.Status == string(domain.RunnerFailed)) {
			failures = append(failures, map[string]any{
				"seq": rr.Seq, "name": rr.Name, "error_code": *rr.ErrorCode,
			})
		}
	}
	counts["total"] = len(b.Runners)

	payload := map[string]any{
		"schema_version": SchemaVersion2,
		"task_id":        b.Task.ID,
		"name":           b.Task.Name,
		"status":         b.Task.Status,
		"counts":         counts,
		"runners":        runners,
		"failures":       failures,
		"cleanup_failed": cleanupFailed,
	}
	if b.Attempt != nil {
		payload["attempt_no"] = b.Attempt.AttemptNo
		payload["attempt_trigger"] = b.Attempt.Trigger
	}
	if b.Task.StartedAt != nil && b.Task.FinishedAt != nil {
		payload["duration_ms"] = b.Task.FinishedAt.Sub(*b.Task.StartedAt).Milliseconds()
	}
	if len(b.Task.Error) > 0 {
		var f domain.Failure
		if err := json.Unmarshal(b.Task.Error, &f); err == nil && f.Code != "" {
			payload["error"] = f
		}
	}
	return payload
}

// ---- report.md（C-7.3 模板：状态/耗时/管道表/失败清单/链接；不渲染指标图表）-----

func buildReportMarkdown(b *store.TaskReportBundle, now time.Time) string {
	var sb strings.Builder
	status := strings.ToUpper(b.Task.Status)
	fmt.Fprintf(&sb, "# Task %s\n\n", b.Task.ID)
	fmt.Fprintf(&sb, "- 状态：%s", status)
	if b.Attempt != nil {
		fmt.Fprintf(&sb, "（attempt #%d，%s）", b.Attempt.AttemptNo, b.Attempt.Trigger)
	}
	sb.WriteString("\n")
	if b.Task.StartedAt != nil && b.Task.FinishedAt != nil {
		fmt.Fprintf(&sb, "- 耗时：%s\n", b.Task.FinishedAt.Sub(*b.Task.StartedAt).Round(time.Second))
	}
	fmt.Fprintf(&sb, "- 生成时间：%s\n\n", now.Format(time.RFC3339))

	sb.WriteString("## runner 管道\n\n")
	sb.WriteString("| seq | 名称 | 角色 | 状态 | 错误码 |\n")
	sb.WriteString("|---|---|---|---|---|\n")
	for _, rr := range b.Runners {
		code := "-"
		if rr.ErrorCode != nil {
			code = *rr.ErrorCode
		}
		fmt.Fprintf(&sb, "| %d | %s | %s | %s | %s |\n", rr.Seq, rr.Name, rr.Role, rr.Status, code)
	}

	failed := false
	for _, rr := range b.Runners {
		if rr.Status == string(domain.RunnerFailed) {
			failed = true
			break
		}
	}
	if failed {
		sb.WriteString("\n## 失败清单\n\n")
		for _, rr := range b.Runners {
			if rr.Status != string(domain.RunnerFailed) {
				continue
			}
			code := "UNKNOWN"
			if rr.ErrorCode != nil {
				code = *rr.ErrorCode
			}
			fmt.Fprintf(&sb, "- seq=%d %s：%s\n", rr.Seq, rr.Name, code)
		}
	}
	sb.WriteString("\n## 产物与报告\n\n")
	sb.WriteString("- `report.json` / `metadata.json` / `events.jsonl`\n")
	sb.WriteString("- 产物目录：`attempts/<n>/runners/<seq>/`（result.json / result.html / result.csv 三选一之一 + raw/ + logs/，D-74）\n")
	return sb.String()
}

// ---- metadata.json：裁决快照（版本、镜像引用 + 实际 digest、Profile、Manifest 原文）---

func buildMetadata(b *store.TaskReportBundle) map[string]any {
	meta := map[string]any{
		"task_id":   b.Task.ID,
		"name":      b.Task.Name,
		"status":    b.Task.Status,
		"manifest":  store.JSONOrNil(b.Task.Manifest),
		"resolved":  store.JSONOrNil(b.Task.Resolved),
		"submitted": b.Task.SubmittedAt.UTC().Format(time.RFC3339),
	}
	if b.Task.RetryOf != nil {
		meta["retry_of"] = *b.Task.RetryOf
	}
	runners := make([]map[string]any, 0, len(b.Runners))
	for _, rr := range b.Runners {
		item := map[string]any{
			"seq":             rr.Seq,
			"runner":          rr.RunnerName + "@" + rr.RunnerVersion,
			"image_ref":       rr.ImageRef,
			"image_digest":    "-", // 缺失记 unknown（"同 tag 是否换了代码"的唯一证据，D-36）
			"profile":         rr.Profile,
			"input_ref":       rr.InputRef,
			"timeout_seconds": rr.TimeoutSeconds,
		}
		if rr.ImageDigest != nil {
			item["image_digest"] = *rr.ImageDigest
		}
		runners = append(runners, item)
	}
	meta["runners"] = runners
	if b.Attempt != nil {
		meta["attempt_no"] = b.Attempt.AttemptNo
	}
	return meta
}

// ---- events.jsonl（C-7.4：库是权威；writer 按 seq 追加，重启后按 seq 幂等补齐）----

func writeEventsJSONL(path string, events []store.Event) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("report: 写 events.jsonl: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range events {
		if err := enc.Encode(eventLine(e)); err != nil {
			return fmt.Errorf("report: encode event seq=%d: %w", e.Seq, err)
		}
	}
	return nil
}

func eventLine(e store.Event) map[string]any {
	line := map[string]any{
		"seq":     e.Seq,
		"type":    e.Type,
		"task_id": e.TaskID,
		"at":      e.CreatedAt.UTC().Format(time.RFC3339Nano),
		"payload": e.Payload,
	}
	if e.AttemptNo != nil {
		line["attempt_no"] = *e.AttemptNo
	}
	if e.RunnerSeq != nil {
		line["runner_seq"] = *e.RunnerSeq
	}
	if e.FromStatus != nil {
		line["from"] = *e.FromStatus
	}
	if e.ToStatus != nil {
		line["to"] = *e.ToStatus
	}
	if e.Reason != nil {
		line["reason"] = *e.Reason
	}
	if e.Actor != nil {
		line["actor"] = *e.Actor
	}
	return line
}

// ---- 产物索引重建（派生数据；路径相对 <storage-root>/<task_id>/，C-7.5）---------

func (w *Writer) indexArtifacts(ctx context.Context, taskID, dir string) error {
	var rows []store.Artifact
	root := filepath.Join(w.Root, taskID)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件不可读不阻塞索引重建（可重试）
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		att, seq := artifactOwners(rel)
		rows = append(rows, store.Artifact{
			TaskID:    taskID,
			AttemptNo: att,
			RunnerSeq: seq,
			Path:      rel,
			SizeBytes: info.Size(),
			Kind:      classifyArtifact(rel),
			MTime:     ptrTime(info.ModTime()),
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return w.Store.Artifacts.UpsertBatch(ctx, tx, taskID, rows)
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

func classifyArtifact(rel string) string {
	switch {
	case strings.HasSuffix(rel, "result.json"), strings.HasSuffix(rel, "result.html"), strings.HasSuffix(rel, "result.csv"):
		return "result"
	case strings.Contains(rel, "/logs/"), strings.HasSuffix(rel, "events.jsonl"):
		return "log"
	case strings.HasSuffix(rel, "report.json"), strings.HasSuffix(rel, "report.md"), strings.HasSuffix(rel, "metadata.json"):
		return "report"
	case strings.HasSuffix(rel, ".exports.env"):
		return "export"
	case strings.Contains(rel, "/input/"):
		return "other"
	default:
		return "raw"
	}
}

// artifactOwners 从相对路径解析 (attempt_no, runner_seq)：attempts/<n>/... 与
// attempts/<n>/runners/<seq>/...（§8.1）。
func artifactOwners(rel string) (*int, *int) {
	var att, seq *int
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[0] == "attempts" {
		if n, err := strconvAtoi(parts[1]); err == nil {
			att = &n
		}
		for i, p := range parts {
			if p == "runners" && i+1 < len(parts) {
				if n, err := strconvAtoi(parts[i+1]); err == nil {
					seq = &n
				}
			}
		}
	}
	return att, seq
}

func strconvAtoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// fileSHA256 供调用方按需取文件摘要（索引里 sha256 惰性/可选，C-7.5）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
