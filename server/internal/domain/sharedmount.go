package domain

import (
	"fmt"
	"path"
	"strings"
)

// 统一共享挂载（D-63，票 #66 地图 / #70 合并规则 / #74 保留路径合并）。
//
// 一句话：平台只认 **PVC 名**（claimName），不理解 NFS/CephFS/CSI 协议字段（ARCHITECTURE §10.4）。
// 内联 `volumes[].nfs` 会被 PodSecurity `restricted:latest` 拒绝（研究 #69 实测），
// 所以平台**不新增 volume source 类型**，统一挂载复用 MountSpec.Source 那条 PVC 路径。

// SharedMountSpec 是平台级统一挂载的一条配置（Helm values → 平台配置，**不来自 tester.yaml**）。
type SharedMountSpec struct {
	// Name 是 volume 名（渲染前会 sanitize 并去重）。
	Name string `json:"name" yaml:"name"`
	// MountPath 是容器内挂载点（绝对路径）。
	MountPath string `json:"mountPath" yaml:"mountPath"`
	// ReadOnly 默认 true：模型权重是只读输入，默认不给写。
	ReadOnly bool `json:"readOnly" yaml:"readOnly"`
	// ClaimName 是集群里已存在的 PVC 名（平台不解析它背后的存储协议）。
	ClaimName string `json:"claimName" yaml:"claimName"`
}

// 平台保留挂载点（**唯一常量集**）。
//
// #74：之前有两套不一致的集合 —— registry.go 的 reservedMountPaths 有 `/tester`，
// render.go 的 used 集合没有 `/tester`，于是 `/tester` 提交期被拒、渲染期却放行。
// 现在两边都从 here 取。
const (
	// WorkspaceOutputPath 是产物目录（平台写，Tester 写）。
	WorkspaceOutputPath = "/workspace/output"
	// WorkspaceInputPath 是可选输入目录（只读）。
	WorkspaceInputPath = "/workspace/input"
	// TmpPath 配合只读根：/tmp 必须是可写 emptyDir。
	TmpPath = "/tmp"
	// TesterDirPath 是 Tester 镜像目录（入口 /tester/run.sh 所在）。
	TesterDirPath = "/tester"
)

// 默认统一挂载路径（D-63：两条独立条目，可指向同一或不同 PVC）。
const (
	DefaultModelsMountPath    = "/models"
	DefaultAltModelsMountPath = "/mnt/model"
)

// ReservedMountPaths 返回"平台保留 + 统一挂载"的 mount_path 集合（path → 人类可读原因）。
//
// 统一挂载路径也算保留：Tester 自己的 mounts[] 与它们同路径 → 提交期 400（D-63 ⑤）。
func ReservedMountPaths(shared []SharedMountSpec) map[string]string {
	out := map[string]string{
		WorkspaceOutputPath: "platform output directory",
		WorkspaceInputPath:  "platform input directory",
		TmpPath:             "platform tmp directory",
		TesterDirPath:       "tester image directory",
	}
	for _, s := range shared {
		p := cleanMountPath(s.MountPath)
		if p == "" {
			continue
		}
		label := s.Name
		if label == "" {
			label = s.ClaimName
		}
		out[p] = fmt.Sprintf("platform shared mount %q (PVC %q, D-63)", label, s.ClaimName)
	}
	return out
}

// cleanMountPath 归一化比较用的路径（去掉尾部斜杠；空串原样）。
func cleanMountPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if !path.IsAbs(p) {
		return p
	}
	return path.Clean(p)
}

// MountPathConflicts 报告 Tester 自己的 mounts 是否与平台保留/统一挂载冲突。
//
// 返回冲突的 mount_path → 原因（**不做只读交集**：同路径一律拒绝，D-63 ⑤）。
func MountPathConflicts(testerMounts []MountSpec, shared []SharedMountSpec) map[string]string {
	reserved := ReservedMountPaths(shared)
	conflicts := map[string]string{}
	for _, m := range testerMounts {
		p := cleanMountPath(m.MountPath)
		if reason, ok := reserved[p]; ok {
			conflicts[p] = reason
		}
	}
	return conflicts
}

// NormalizeSharedMounts 校验并归一平台级统一挂载配置（启动期调用）。
//
// 校验（D-63）：绝对路径、非平台保留路径、name 唯一、claimName 必填；readOnly 默认 true。
// 返回校验后的副本（不修改入参）。
func NormalizeSharedMounts(in []SharedMountSpec) ([]SharedMountSpec, error) {
	out := make([]SharedMountSpec, 0, len(in))
	names := map[string]int{}
	paths := map[string]int{}
	reservedStatic := map[string]string{
		WorkspaceOutputPath: "platform output directory",
		WorkspaceInputPath:  "platform input directory",
		TmpPath:             "platform tmp directory",
		TesterDirPath:       "tester image directory",
	}
	for i, s := range in {
		item := s
		item.Name = strings.TrimSpace(item.Name)
		item.ClaimName = strings.TrimSpace(item.ClaimName)
		item.MountPath = cleanMountPath(item.MountPath)

		if item.ClaimName == "" {
			return nil, fmt.Errorf("sharedMounts[%d]: claimName 必填（平台只认 PVC 名，D-63）", i)
		}
		if item.MountPath == "" || !path.IsAbs(item.MountPath) {
			return nil, fmt.Errorf("sharedMounts[%d]: mountPath 必须是绝对路径，得到 %q", i, s.MountPath)
		}
		if reason, isReserved := reservedStatic[item.MountPath]; isReserved {
			return nil, fmt.Errorf("sharedMounts[%d]: mountPath %q 与平台保留路径冲突（%s）", i, item.MountPath, reason)
		}
		if item.Name == "" {
			item.Name = strings.TrimPrefix(strings.ReplaceAll(item.MountPath, "/", "-"), "-")
			if item.Name == "" {
				return nil, fmt.Errorf("sharedMounts[%d]: name 不能推导（mountPath=%q），请显式给出", i, item.MountPath)
			}
		}
		if prev, dup := names[item.Name]; dup {
			return nil, fmt.Errorf("sharedMounts[%d]: name %q 与 sharedMounts[%d] 重复", i, item.Name, prev)
		}
		if prev, dup := paths[item.MountPath]; dup {
			return nil, fmt.Errorf("sharedMounts[%d]: mountPath %q 与 sharedMounts[%d] 重复", i, item.MountPath, prev)
		}
		names[item.Name] = i
		paths[item.MountPath] = i
		// readOnly 的"默认 true"在**配置解析层**完成（那里能区分"未写"与"显式 false"）；
		// 到这里时 bool 已经是最终值，不再改写。
		out = append(out, item)
	}
	return out, nil
}
