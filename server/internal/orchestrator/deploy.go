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
	if err := c.deployer.Cancel(ctx, runID); err != nil {
		record(&domain.DeployUninstall{
			State: "failed", Force: true, CleanupErrors: []string{err.Error()},
		})
		c.logf("orchestrator: WARN deploy_cleanup_failed: run %s: %v", runID, err)
		return
	}
	// 只有声明支持校验的适配层才做"删干净"判定；否则退化为 DELETE 成功即 verified。
	if cleaner, ok := c.deployer.(ports.DeployCleaner); ok {
		gone, err := cleaner.VerifyUninstalled(ctx, runID)
		switch {
		case err != nil:
			record(&domain.DeployUninstall{
				State: "failed", Force: true, CleanupErrors: []string{err.Error()},
			})
			c.logf("orchestrator: WARN deploy_cleanup_verify: run %s: %v", runID, err)
			return
		case !gone:
			record(&domain.DeployUninstall{
				State: "failed", Force: true, CleanupErrors: []string{"release still present after uninstall"},
			})
			c.logf("orchestrator: WARN deploy_cleanup_verify: run %s: release still present", runID)
			return
		}
	}
	record(&domain.DeployUninstall{
		State: "verified", VerifiedAt: &now, Force: true, CleanupErrors: []string{},
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
