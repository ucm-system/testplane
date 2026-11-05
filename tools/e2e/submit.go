package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// submitOutcome 是 POST /runs 的结果。
type submitOutcome struct {
	RunID  string
	Status int
	Body   []byte
}

// submit 提交 Run：POST /runs，断言 202 + run_id（07 §4.2）。
// 可选先 dry-run（--dry-run-first，默认关）：设计上 dry_run 只校验不落库，
// 但 MVP 是否实现该查询参数尚未确认，默认关掉避免"多建一个 Run"。
func (a *App) submit(ctx context.Context) (*submitOutcome, error) {
	raw, contentType, err := a.loadManifest()
	if err != nil {
		return nil, err
	}
	if a.opts.Verbose && a.cfg.Logf != nil {
		a.cfg.Logf("Manifest（%s，%d 字节）：\n%s", contentType, len(raw), string(raw))
	}

	if a.opts.DryRunFirst {
		if err := a.dryRun(ctx, raw, contentType); err != nil {
			return nil, err
		}
	}

	status, header, body, err := a.client.Post(ctx, "/runs", raw, contentType)
	if err != nil {
		return nil, err
	}
	_ = header
	if err := expectStatus(status, 202, body); err != nil {
		return nil, fmt.Errorf("提交 Run 失败：%w", err)
	}

	runID := extractRunID(body)
	if runID == "" {
		return nil, fmt.Errorf("202 响应里没有 run id（期望 {\"run\":{\"id\":\"r-...\"}}）：%s", truncate(string(body), 800))
	}
	a.addCheck("submit.http_status", true, "HTTP 202 Accepted")
	a.addCheck("submit.run_id", true, runID)
	if !responseHasTasks(body) {
		a.warn("202 响应里没有 tasks[]（07 §4.2 期望同时返回 Task 摘要）")
	}
	return &submitOutcome{RunID: runID, Status: status, Body: body}, nil
}

func (a *App) dryRun(ctx context.Context, raw []byte, contentType string) error {
	status, header, body, err := a.client.Post(ctx, "/runs?dry_run=true", raw, contentType)
	if err != nil {
		return err
	}
	_ = header
	if status == 404 {
		return &UsageError{Msg: "开启了 --dry-run-first，但控制面没有 POST /runs?dry_run（404）；去掉该参数后重试"}
	}
	if err := expectStatus(status, 200, body); err != nil {
		return fmt.Errorf("dry-run 失败：%w", err)
	}

	var res struct {
		Valid  *bool `json:"valid"`
		Issues []struct {
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("dry-run 响应不是合法 JSON：%v；body=%s", err, truncate(string(body), 600))
	}
	if res.Valid == nil {
		return &UsageError{Msg: "dry-run 响应缺 valid 字段（后端可能未实现 ?dry_run，请去掉 --dry-run-first）；body=" + truncate(string(body), 400)}
	}
	if !*res.Valid {
		lines := make([]string, 0, len(res.Issues))
		for _, issue := range res.Issues {
			lines = append(lines, fmt.Sprintf("%s: %s（%s）", issue.Path, issue.Message, issue.Code))
		}
		return fmt.Errorf("Manifest 校验未通过（dry-run valid=false）：\n  %s", strings.Join(lines, "\n  "))
	}
	a.addCheck("submit.dry_run", true, "valid=true")
	return nil
}

// extractRunID 兼容 07 §4.2 的 {run:{id}}，以及常见的扁平形态。
func extractRunID(raw []byte) string {
	var resp struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		RunID string `json:"run_id"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	switch {
	case resp.Run.ID != "":
		return resp.Run.ID
	case resp.RunID != "":
		return resp.RunID
	default:
		return resp.ID
	}
}

func responseHasTasks(raw []byte) bool {
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false
	}
	return len(resp.Tasks) > 0
}
