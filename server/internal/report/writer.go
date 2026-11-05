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

// Writer 实现 ports.ReportWriter：把一个 Run 的库内终态写成文件产物（C-7.2）。
//
// 文件布局（06 §3.1）：
//
//	<storage-root>/<run_id>/report.json  report.md  metadata.json  events.jsonl
//
// 幂等：同名文件覆盖写；events.jsonl 按 seq 全量重写（库是权威）。
type Writer struct {
	// Store 是唯一数据来源（库优先，不依赖"文件先于状态存在"）。
	Store *store.Store
	// Root 是 <storage-root>。
	Root string
	// MaxReportBytes 是单 Run 报告上限（默认 20 MiB，超限写摘要 + report.full.json）。
	MaxReportBytes int64
	Logger         *slog.Logger
	// Now 便于测试注入。
	Now func() time.Time
}

var _ interface {
	WriteRunReport(ctx context.Context, runID string) (path string, err error)
} = (*Writer)(nil)

// WriteRunReport 生成 report.json / report.md / metadata.json / events.jsonl 并重建产物索引。
// 返回 report.json 的绝对路径。
func (w *Writer) WriteRunReport(ctx context.Context, runID string) (string, error) {
	lg := w.Logger
	if lg == nil {
		lg = slog.Default()
	}
	if w.Store == nil {
		return "", fmt.Errorf("report: writer 缺少 store")
	}
	bundle, err := w.Store.ReportBundle(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("report: 读 Run %s: %w", runID, err)
	}

	dir := filepath.Join(w.Root, runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("report: 建目录 %s: %w", dir, err)
	}

	// 1. events.jsonl：按 seq 全量重写（库是权威，重启可重建，C-7.4）
	if err := writeEventsJSONL(filepath.Join(dir, "events.jsonl"), bundle.Events); err != nil {
		return "", err
	}

	// 2. report.json（超限时写摘要 + report.full.json）
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
			"run_id":         payload["run_id"],
			"status":         payload["status"],
			"counts":         payload["counts"],
			"truncated":      true,
			"report_ref":     "report.full.json",
			"schema_version": "1.0",
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

	// 3. report.md（C-7.3 模板）
	md := buildReportMarkdown(bundle, w.now())
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(md), 0o640); err != nil {
		return "", fmt.Errorf("report: 写 report.md: %w", err)
	}

	// 4. metadata.json：裁决快照（版本、镜像引用 + 实际 digest、Profile、Endpoint、Manifest 原文）
	meta, err := json.MarshalIndent(buildMetadata(bundle), "", "  ")
	if err != nil {
		return "", fmt.Errorf("report: marshal metadata.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), meta, 0o640); err != nil {
		return "", fmt.Errorf("report: 写 metadata.json: %w", err)
	}

	// 5. 产物索引（可重建）：扫该 Run 目录并写 artifacts
	if err := w.indexArtifacts(ctx, runID, dir); err != nil {
		lg.Warn("产物索引失败（不影响报告）", "err", err, "run_id", runID)
	}

	lg.Info("报告已生成", "run_id", runID, "path", reportPath)
	return reportPath, nil
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// ---- report.json ---------------------------------------------------------

func buildReportJSON(b *store.RunBundle) map[string]any {
	counts := map[string]int{
		"total": 0, "succeeded": 0, "failed": 0, "skipped": 0, "canceled": 0,
	}
	tasks := make([]map[string]any, 0, len(b.Tasks))
	failures := make([]map[string]any, 0)
	for i := range b.Tasks {
		t := &b.Tasks[i]
		counts["total"]++
		switch domain.TaskStatus(t.Status) {
		case domain.TaskSucceeded:
			counts["succeeded"]++
		case domain.TaskFailed:
			counts["failed"]++
		case domain.TaskSkipped:
			counts["skipped"]++
		case domain.TaskCanceled:
			counts["canceled"]++
		}
		attempts := b.Attempts[t.ID]
		var durationMS *int64
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			if last.StartedAt != nil && last.FinishedAt != nil {
				ms := last.FinishedAt.Sub(*last.StartedAt).Milliseconds()
				durationMS = &ms
			}
		}
		item := map[string]any{
			"task_id":  t.ID,
			"name":     t.Name,
			"status":   t.Status,
			"attempts": len(attempts),
		}
		if t.ResultRef != nil {
			item["result_ref"] = *t.ResultRef
		}
		if durationMS != nil {
			item["duration_ms"] = *durationMS
		}
		tasks = append(tasks, item)

		if domain.TaskStatus(t.Status) != domain.TaskSucceeded {
			reason := ""
			if t.StatusReason != nil {
				reason = *t.StatusReason
			}
			code := ""
			if len(t.Error) > 0 {
				var f domain.Failure
				if err := json.Unmarshal(t.Error, &f); err == nil {
					code = f.Code
					if reason == "" {
						reason = f.Code
					}
				}
			}
			if code == "" && len(t.Error) > 0 {
				var raw map[string]any
				if err := json.Unmarshal(t.Error, &raw); err == nil {
					if c, ok := raw["code"].(string); ok {
						code = c
					}
				}
			}
			failures = append(failures, map[string]any{
				"task_id":    t.ID,
				"name":       t.Name,
				"status":     t.Status,
				"reason":     reason,
				"error_code": code,
			})
		}
	}

	out := map[string]any{
		"run_id":         b.Run.ID,
		"name":           b.Run.Name,
		"kind":           b.Run.Kind,
		"status":         b.Run.Status,
		"submitted_at":   b.Run.SubmittedAt.UTC(),
		"finished_at":    utcOrNil(b.Run.FinishedAt),
		"counts":         counts,
		"tasks":          tasks,
		"failures":       failures,
		"schema_version": "1.0",
	}
	if len(b.Run.Error) > 0 {
		out["error"] = json.RawMessage(b.Run.Error)
	}
	return out
}

// ---- report.md（C-7.3：状态 / 耗时 / Task 表 / 失败清单 / 产物与报告链接）---

func buildReportMarkdown(b *store.RunBundle, now time.Time) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Run %s\n\n", b.Run.ID)
	fmt.Fprintf(&sb, "- 名称：`%s`\n", b.Run.Name)
	fmt.Fprintf(&sb, "- 状态：**%s**\n", strings.ToUpper(b.Run.Status))
	fmt.Fprintf(&sb, "- 类型：%s\n", b.Run.Kind)
	fmt.Fprintf(&sb, "- 提交人：%s\n", b.Run.SubmittedBy)
	fmt.Fprintf(&sb, "- 提交时间：%s\n", b.Run.SubmittedAt.UTC().Format(time.RFC3339))
	if b.Run.FinishedAt != nil {
		fmt.Fprintf(&sb, "- 结束时间：%s\n", b.Run.FinishedAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(&sb, "- 总耗时：%s\n", b.Run.FinishedAt.Sub(b.Run.SubmittedAt).Round(time.Second))
	}
	if len(b.Run.Error) > 0 {
		fmt.Fprintf(&sb, "- 错误：`%s`\n", string(b.Run.Error))
	}
	sb.WriteString("\n## Task\n\n")
	sb.WriteString("| # | Task | 名称 | Tester | 状态 | Attempts | 错误码 |\n")
	sb.WriteString("|---|---|---|---|---|---|---|\n")
	for i := range b.Tasks {
		t := &b.Tasks[i]
		code := ""
		if len(t.Error) > 0 {
			var f domain.Failure
			if err := json.Unmarshal(t.Error, &f); err == nil {
				code = f.Code
			}
		}
		fmt.Fprintf(&sb, "| %d | `%s` | %s | %s@%s | %s | %d | %s |\n",
			t.Seq, t.ID, t.Name, t.TesterName, t.TesterVersion, t.Status,
			len(b.Attempts[t.ID]), code)
	}

	sb.WriteString("\n## 失败与跳过\n\n")
	anyFailure := false
	for i := range b.Tasks {
		t := &b.Tasks[i]
		status := domain.TaskStatus(t.Status)
		if status == domain.TaskSucceeded {
			continue
		}
		anyFailure = true
		reason := ""
		if t.StatusReason != nil {
			reason = *t.StatusReason
		}
		fmt.Fprintf(&sb, "- `%s` %s：%s %s\n", t.ID, t.Name, t.Status, reason)
	}
	if !anyFailure {
		sb.WriteString("- 无\n")
	}

	sb.WriteString("\n## 产物与报告\n\n")
	sb.WriteString("| 文件 | 说明 |\n|---|---|\n")
	sb.WriteString("| `report.json` | Run 汇总（组织与索引，不做指标聚合） |\n")
	sb.WriteString("| `metadata.json` | 裁决快照：镜像 digest、Profile、Endpoint、Manifest |\n")
	sb.WriteString("| `events.jsonl` | 全量状态事件（按 seq） |\n")
	for i := range b.Tasks {
		t := &b.Tasks[i]
		if t.ResultRef != nil {
			fmt.Fprintf(&sb, "| `%s` | Task %s 的 result.json |\n", *t.ResultRef, t.ID)
		}
	}
	fmt.Fprintf(&sb, "\n_生成时间：%s_\n", now.UTC().Format(time.RFC3339))
	return sb.String()
}

// ---- metadata.json -------------------------------------------------------

func buildMetadata(b *store.RunBundle) map[string]any {
	tasks := make([]map[string]any, 0, len(b.Tasks))
	for i := range b.Tasks {
		t := &b.Tasks[i]
		attemptImages := make([]map[string]any, 0, len(b.Attempts[t.ID]))
		for _, a := range b.Attempts[t.ID] {
			item := map[string]any{"attempt_id": a.ID, "idx": a.Idx}
			if a.ImageDigest != nil {
				item["image_digest"] = *a.ImageDigest
			}
			attemptImages = append(attemptImages, item)
		}
		tasks = append(tasks, map[string]any{
			"task_id":         t.ID,
			"name":            t.Name,
			"tester":          t.TesterName + "@" + t.TesterVersion,
			"image_ref":       t.ImageRef,
			"profile":         t.Profile,
			"timeout_seconds": t.TimeoutSeconds,
			"attempts":        attemptImages,
		})
	}
	out := map[string]any{
		"run_id":         b.Run.ID,
		"kind":           b.Run.Kind,
		"status":         b.Run.Status,
		"schema_version": "1.0",
		"tasks":          tasks,
	}
	if len(b.Run.Manifest) > 0 {
		out["manifest"] = json.RawMessage(b.Run.Manifest)
	}
	if len(b.Run.Endpoint) > 0 {
		out["endpoint"] = json.RawMessage(b.Run.Endpoint)
	}
	if len(b.Run.Deploy) > 0 {
		out["deploy"] = json.RawMessage(b.Run.Deploy)
	}
	if len(b.Run.Resolved) > 0 {
		out["resolved"] = json.RawMessage(b.Run.Resolved)
	}
	return out
}

// ---- events.jsonl（C-7.4）-------------------------------------------------

func writeEventsJSONL(path string, events []store.Event) error {
	var sb strings.Builder
	for _, e := range events {
		line := map[string]any{
			"seq":        e.Seq,
			"type":       e.Type,
			"run_id":     e.RunID,
			"at":         e.CreatedAt.UTC(),
			"task_id":    e.TaskID,
			"attempt_id": e.AttemptID,
			"from":       e.FromStatus,
			"to":         e.ToStatus,
			"reason":     e.Reason,
			"actor":      e.Actor,
		}
		if len(e.Payload) > 0 {
			line["payload"] = json.RawMessage(e.Payload)
		}
		b, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("report: marshal event %d: %w", e.Seq, err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	// 先写临时文件再 rename：避免读到半行。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o640); err != nil {
		return fmt.Errorf("report: 写 events.jsonl: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("report: replace events.jsonl: %w", err)
	}
	return nil
}

// ---- 产物索引 ------------------------------------------------------------

// indexArtifacts 扫 <root>/<run_id> 下全部文件并重建 artifacts 索引（C-7.5）。
func (w *Writer) indexArtifacts(ctx context.Context, runID, dir string) error {
	var rows []store.Artifact
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(w.Root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		kind := classifyArtifact(rel)
		mtime := info.ModTime().UTC()
		sha := ""
		if info.Size() <= 1<<20 { // 小文件算 sha256；大文件惰性（C-7.5）
			if sum, err := fileSHA256(path); err == nil {
				sha = sum
			}
		}
		row := store.Artifact{
			RunID: runID, Path: rel, SizeBytes: info.Size(), Kind: kind, MTime: &mtime,
		}
		if sha != "" {
			row.SHA256 = &sha
		}
		if tid, attempt := artifactOwners(rel); tid != "" {
			row.TaskID = &tid
			if attempt != "" {
				row.AttemptID = &attempt
			}
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return w.Store.InTx(ctx, func(tx pgx.Tx) error {
		return w.Store.Artifacts.UpsertBatch(ctx, tx, runID, rows)
	})
}

func classifyArtifact(rel string) string {
	switch {
	case strings.HasSuffix(rel, "/result.json"):
		return "result"
	case strings.HasSuffix(rel, "/report.json"), strings.HasSuffix(rel, "/report.md"),
		strings.HasSuffix(rel, "/metadata.json"), strings.HasSuffix(rel, "/events.jsonl"):
		return "report"
	case strings.Contains(rel, "/logs/"):
		return "log"
	case strings.Contains(rel, "/raw/"):
		return "raw"
	}
	return "other"
}

// artifactOwners 从相对路径里解析 task/attempt 归属：<run>/tasks/<task>/attempts/<n>/...
func artifactOwners(rel string) (taskID, attemptID string) {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == "tasks" && i+1 < len(parts) {
			taskID = parts[i+1]
			if i+3 < len(parts) && parts[i+2] == "attempts" {
				attemptID = taskID + "-a" + parts[i+3]
			}
		}
	}
	return taskID, attemptID
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func utcOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
