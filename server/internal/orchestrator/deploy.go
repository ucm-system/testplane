package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// DeployManifest 是 `runs.deploy` 里编排层自己关心的那几个字段。
// 其余字段（values / set / no_write …）不解析，原样作为 Manifest 透传给 Deployer.Start。
type DeployManifest struct {
	DeployID     string `json:"deployment_id"`
	Provider     string `json:"provider"`
	Namespace    string `json:"namespace"`
	ReleaseName  string `json:"release_name"`
	PackageType  string `json:"package_type"`
	Chart        string `json:"chart"`
	ChartVersion string `json:"chart_version"`
	ValuesDigest string `json:"values_digest"`
	ModelName    string `json:"model_name"`
}

// DeployManifestFields 从 RunView.DeployJSON 里取编排层要落库的冻结字段。
// 解析失败只返回零值（不影响部署：Start 自己会校验 manifest）。
func DeployManifestFields(raw json.RawMessage) DeployManifest {
	var m DeployManifest
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

// DeployReleaseName 是 Genet release 的幂等键（execution §9.2）：
// `tp-<sha256(run_id:deploy_id)[:12]>`，同 run 同 deploy 恒等，且满足 `^[a-z][a-z0-9-]{0,52}$`。
//
// 与 `internal/genet.ReleaseName` 必须**逐字一致**（有单测交叉校验）；编排层不 import
// genet 包（那是外部 IO 适配层），所以这里保留一份实现。
func DeployReleaseName(runID, deployID string) string {
	sum := sha256.Sum256([]byte(runID + ":" + deployID))
	return "tp-" + hex.EncodeToString(sum[:])[:12]
}

// cleanupDeploy 收口一个 Run 的 Genet 资源（§9.6/§9.7）：
//
//  1. DELETE release（对 Deployer 就是 Cancel）；
//  2. 校验"删干净"：GET release 404 且 pods 为空（适配层实现 ports.DeployCleaner 时）；
//  3. 写 runs.deploy.uninstall{state, verified_at, force, cleanup_errors[]}；
//     卸载失败/未通过校验**不改 Run 终态**，记 uninstall.state=failed，由对账重试。
func (c *Controller) cleanupDeploy(ctx context.Context, runID string) {
	if c.deployer == nil {
		return
	}
	now := time.Now().UTC()
	record := func(u *domain.DeployUninstall) {
		_ = c.repos.RecordDeployState(ctx, runID, domain.DeploySnapshot{Uninstall: u})
	}
	// 已 verified 的不再打 Genet（对账每轮都会路过这里）。
	if run, err := c.repos.GetRun(ctx, runID); err == nil && uninstallState(run.DeployJSON) == domain.UninstallVerified {
		return
	}
	cleaner, canVerify := c.deployer.(ports.DeployCleaner)
	verify := func() bool {
		if !canVerify {
			return true // 适配层不支持校验时退化为"DELETE 成功即干净"
		}
		gone, err := cleaner.VerifyUninstalled(ctx, runID)
		return err == nil && gone
	}
	if err := c.deployer.Cancel(ctx, runID); err != nil {
		// DELETE 404 = release 已经不存在（可能上一轮已删掉、或 Genet 侧 GC 掉了）：
		// 用"删干净"判据复核，真的没了就记 verified，别让对账无限重试（§9.7）。
		if verify() {
			record(&domain.DeployUninstall{State: domain.UninstallVerified, VerifiedAt: &now, Force: true, CleanupErrors: []string{}})
			c.logf("orchestrator: deploy_cleanup already gone: run %s", runID)
			return
		}
		record(&domain.DeployUninstall{
			State: domain.UninstallFailed, Force: true, CleanupErrors: []string{err.Error()},
		})
		c.appendCleanupFailed(ctx, runID, []string{err.Error()})
		c.logf("orchestrator: WARN deploy_cleanup_failed: run %s: %v", runID, err)
		return
	}
	// 只有声明支持校验的适配层才做"删干净"判定；否则退化为 DELETE 成功即 verified。
	if canVerify {
		gone, err := cleaner.VerifyUninstalled(ctx, runID)
		switch {
		case err != nil:
			record(&domain.DeployUninstall{
				State: domain.UninstallFailed, Force: true, CleanupErrors: []string{err.Error()},
			})
			c.appendCleanupFailed(ctx, runID, []string{err.Error()})
			c.logf("orchestrator: WARN deploy_cleanup_verify: run %s: %v", runID, err)
			return
		case !gone:
			record(&domain.DeployUninstall{
				State: domain.UninstallFailed, Force: true, CleanupErrors: []string{"release still present after uninstall"},
			})
			c.appendCleanupFailed(ctx, runID, []string{"release still present after uninstall"})
			c.logf("orchestrator: WARN deploy_cleanup_verify: run %s: release still present", runID)
			return
		}
	}
	record(&domain.DeployUninstall{
		State: domain.UninstallVerified, VerifiedAt: &now, Force: true, CleanupErrors: []string{},
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
// uninstallState 读 runs.deploy.uninstall.state（缺省为空串）。
func uninstallState(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var doc struct {
		Uninstall struct {
			State string `json:"state"`
		} `json:"uninstall"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return doc.Uninstall.State
}
// appendCleanupFailed 写一条 cleanup.failed 事件（只用于卸载失败；载荷见 03-data-view §4.6）。
func (c *Controller) appendCleanupFailed(ctx context.Context, runID string, cleanupErrors []string) {
	payload := map[string]any{
		"release_name":   "",
		"cleanup_errors": cleanupErrors,
		"attempt":        nil, // 清理发生在 Run 级，没有"某一次 Attempt"可指；键保留以便前端统一渲染
		"verified_at":    nil,
	}
	if run, err := c.repos.GetRun(ctx, runID); err == nil {
		payload["release_name"] = releaseNameOf(run.DeployJSON)
	}
	if err := c.repos.AppendRunEvent(ctx, runID, domain.EventCleanupFailed, payload); err != nil {
		c.logf("orchestrator: WARN append cleanup.failed event: run %s: %v", runID, err)
	}
}

// releaseNameOf 读 runs.deploy.release_name（缺失返回空串）。
func releaseNameOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var doc struct {
		ReleaseName string `json:"release_name"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return doc.ReleaseName
}