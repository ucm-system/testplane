package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// SeedDefaults 是种子数据（03 §9"首次 seed"的简化）：
// 在 clusters / execution_profiles **表为空**时各插一行，非空则不插（幂等）。
//
// 为什么需要：e2e 与设计示例都用 `profile: k8s-npu`，没有种子时提交会
// EXECUTION_PROFILE_NOT_FOUND（C-5.1）。
//
// 注意：03 §3.2 的 execution_profiles **没有 storage_pvc 列**（PVC 名来自 Helm/Profile
// 挂载配置），所以这里不写 storage_pvc，mounts 留空数组。
type SeedOptions struct {
	ClusterName          string
	APIServer            string
	ProfileName          string
	Namespace            string
	DeployTimeoutSeconds int
}

// DefaultSeedOptions 返回设计示例里的默认种子（k8s-npu / cluster-01）。
func DefaultSeedOptions() SeedOptions {
	apiServer := strings.TrimSpace(os.Getenv("AG_SEED_API_SERVER"))
	if apiServer == "" {
		apiServer = "https://kubernetes.default.svc"
	}
	ns := strings.TrimSpace(os.Getenv("AG_EXECUTOR_NAMESPACE"))
	if ns == "" {
		ns = "argus"
	}
	return SeedOptions{
		ClusterName:          "cluster-01",
		APIServer:            apiServer,
		ProfileName:          "k8s-npu",
		Namespace:            ns,
		DeployTimeoutSeconds: 259200, // 72h（K-6）
	}
}

// SeedDefaults 按 DefaultSeedOptions 播种；表非空则什么都不做。
func (s *Store) SeedDefaults(ctx context.Context) error {
	o := DefaultSeedOptions()
	o.ClusterName = s.ClusterName()
	return s.SeedDefaultsWith(ctx, o)
}

// SeedDefaultsWith 是 SeedDefaults 的可配置版本（测试与多环境用）。
func (s *Store) SeedDefaultsWith(ctx context.Context, o SeedOptions) error {
	if o.ClusterName == "" {
		o.ClusterName = "cluster-01"
	}
	if o.APIServer == "" {
		o.APIServer = "https://kubernetes.default.svc"
	}
	if o.ProfileName == "" {
		o.ProfileName = "k8s-npu"
	}
	if o.Namespace == "" {
		o.Namespace = "argus"
	}
	if o.DeployTimeoutSeconds <= 0 {
		o.DeployTimeoutSeconds = 259200
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		var clusters int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM clusters`).Scan(&clusters); err != nil {
			return fmt.Errorf("store: seed: count clusters: %w", err)
		}
		if clusters == 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO clusters (name, api_server, kubeconfig_key, description)
				VALUES ($1, $2, '', $3)
				ON CONFLICT (name) DO NOTHING`,
				o.ClusterName, o.APIServer, "默认集群"); err != nil {
				return fmt.Errorf("store: seed cluster: %w", err)
			}
		}

		var profiles int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM execution_profiles`).Scan(&profiles); err != nil {
			return fmt.Errorf("store: seed: count profiles: %w", err)
		}
		if profiles == 0 {
			cluster := o.ClusterName
			// Profile 的 cluster 有外键：若集群被别的记录占用（非空），取任意已存在集群。
			if err := tx.QueryRow(ctx,
				`SELECT name FROM clusters ORDER BY name LIMIT 1`).Scan(&cluster); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("store: seed: pick cluster: %w", err)
				}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO execution_profiles
				  (name, cluster, namespace, mounts, scheduler, image_pull_secrets,
				   resource_defaults, deploy_timeout_seconds, network_policy, created_by)
				VALUES ($1, $2, $3, '[]', 'default', '[]', '{}', $4, '{}', 'system:seed')
				ON CONFLICT (name) DO NOTHING`,
				o.ProfileName, cluster, o.Namespace, o.DeployTimeoutSeconds); err != nil {
				return fmt.Errorf("store: seed profile: %w", err)
			}
		}
		return nil
	})
}

// TaskOutcomeStatus 供 API 层判定终态语义——直接转调 domain 的唯一真源。
func TaskOutcomeStatus(in domain.TaskOutcomeInput) domain.TaskStatus {
	return domain.DecideTaskOutcome(in)
}
