// Package genet 是 Genet 的真实 OpenAPI 客户端（design/impl/execution.md §9）。
//
// Genet 没有"部署单"：**一次部署 = 一次 Helm release**，定位符是 `namespace + releaseName`。
// 本包把这条外部语义适配成 ports.Deployer 的三个方法（Start / Poll / Cancel），
// 并在 Poll 里做 §9.3 的九行状态合成。
//
// 文件分工：
//   - dto.go    —— 请求/响应结构，字段名以 Genet 后端代码为准（不是它的文档）；
//   - client.go —— HTTP 传输、鉴权、CA/自签、错误映射；
//   - genet.go  —— ports.Deployer 实现：幂等拉起、单飞轮询、状态合成、取消；
//   - mocks/    —— kind E2E 用的假实现（TESTPLANE_DEPLOY_PROVIDER=mock）。
package genet

import (
	"encoding/json"
	"time"
)

// 失败码统一用 domain 的闭合枚举（附录 D，domain/codes.go）：
//   - domain.CodeDeployFailed  = DEPLOY_FAILED（平台侧调不通 Genet：网络/5xx/鉴权/请求构造，不可重试，Run failed）
//   - domain.CodeDeployBlocked = DEPLOY_BLOCKED（受理但未就绪 / 超窗 / superseded 等，可重试，Run service_unavailable）
// 本包不再重复定义字符串常量，避免两处漂移。

// Deploy 子状态（与 ports.DeployState.Phase 一致）。
const (
	PhaseQueued    = "queued"
	PhaseAccepted  = "accepted"
	PhaseDeploying = "deploying"
	PhaseReady     = "ready"
	PhaseBlocked   = "blocked"
)

// Helm release 状态（`release.status`，helm 原值）。
const (
	ReleasePendingInstall  = "pending-install"
	ReleasePendingUpgrade  = "pending-upgrade"
	ReleasePendingRollback = "pending-rollback"
	ReleaseDeployed        = "deployed"
	ReleaseFailed          = "failed"
	ReleaseSuperseded      = "superseded"
	ReleaseUninstalling    = "uninstalling"
	ReleaseUninstalled     = "uninstalled"
)

// callInfo.endpoint.status 的三值。
const (
	EndpointPending = "pending"
	EndpointReady   = "ready"
	EndpointBlocked = "blocked"
)

// 默认值（§9.5 的轮询参数；可由 Options 覆盖）。
const (
	DefaultPollInitialInterval = 1 * time.Second
	DefaultPollMaxInterval     = 15 * time.Second
	DefaultRequestTimeout      = 10 * time.Second
	DefaultNotFoundGrace       = 180 * time.Second
	DefaultDeployTimeout       = 72 * time.Hour
	DefaultFastPollWindow      = 30 * time.Second
)

// ---- 请求 ----------------------------------------------------------------

// ReleaseInstall 是 `POST /api/open/helm/uc-packages/{stack|pd}/releases` 的请求体。
// 字段名以 Genet 后端 Go 结构（`UcStackInstallRequest`）为准。
type ReleaseInstall struct {
	ReleaseName  string     `json:"releaseName"`
	Values       string     `json:"values,omitempty"`
	TemplatePath string     `json:"templatePath,omitempty"`
	TemplateID   string     `json:"templateId,omitempty"`
	ModelPath    string     `json:"modelPath,omitempty"`
	Image        string     `json:"image,omitempty"`
	Set          []SetValue `json:"set,omitempty"`
	// NoWait 恒 true：`timeoutSec` 只是 helm `--wait` 的 deadline，
	// 排队超过它 release 会被判 failed 而 Pod 仍在排队（§9.2）。
	NoWait bool `json:"noWait"`
	// Wait 是 noWait 的镜像字段（后端把它折算成 !noWait）；两者都给时以后端为准。
	Wait bool `json:"wait,omitempty"`
	// TimeoutSec 只作后台 goroutine 上限，不当排队窗口。
	TimeoutSec int `json:"timeoutSec,omitempty"`
}

// SetValue 是 helm --set / --set-string / --set-json 的一项。
type SetValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// UninstallRequest 是 `DELETE /releases/{name}` 的 query 参数。
type UninstallRequest struct {
	ReleaseName string
	KeepHistory bool
	Force       bool
}

// ---- 响应 ----------------------------------------------------------------

// ReleaseReceipt 是 202 响应（精简 HelmReleaseDTO）。
type ReleaseReceipt struct {
	Name          string `json:"name"`
	RequestedName string `json:"requestedName"`
	Namespace     string `json:"namespace"`
	PackageType   string `json:"packageType"`
	Chart         string `json:"chart"`
	ChartVersion  string `json:"chartVersion"`
	Status        string `json:"status"`
}

// Release 是 `GET /api/open/helm/releases/{name}` 的响应（HelmReleaseDTO）。
// 不确定或被平台忽略的字段保留 Raw，解析时分发，避免硬编码错名字。
type Release struct {
	Name          string          `json:"name"`
	RequestedName string          `json:"requestedName"`
	Namespace     string          `json:"namespace"`
	PackageType   string          `json:"packageType"`
	Chart         string          `json:"chart"`
	ChartVersion  string          `json:"chartVersion"`
	Revision      int             `json:"revision"`
	Status        string          `json:"status"`
	Description   string          `json:"description"`
	UpdatedAt     string          `json:"updatedAt"`
	CallInfo      *CallInfo       `json:"callInfo"`
	Pods          []Pod           `json:"pods"`
	Values        json.RawMessage `json:"values,omitempty"`
	Manifest      json.RawMessage `json:"manifest,omitempty"`
}

// CallInfo 是后端自己算的调用信息（字段名以 Genet 后端 models/helm.go 为准）。
type CallInfo struct {
	Endpoint   *Endpoint  `json:"endpoint"`
	ServiceName string    `json:"serviceName"`
	Namespace  string     `json:"namespace"`
	Port       int        `json:"port"`
	ClusterURL string     `json:"clusterURL"`
	Model      string     `json:"model"`
	External   []External `json:"external"`
	RouterRequired bool   `json:"routerRequired"`
	RouterAddress  string `json:"routerAddress"`
	RouterURL      string `json:"routerURL"`
	RouterMessage  string `json:"routerMessage"`
}

// Endpoint 是 callInfo.endpoint：**地址解析结果**，不代表能推理（§9.4）。
type Endpoint struct {
	Status  string `json:"status"`
	Source  string `json:"source"`
	URL     string `json:"url"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Message string `json:"message"`
}

// External 是 callInfo.external[] 的一项（hostNetwork 场景）。
type External struct {
	PodName  string `json:"podName"`
	NodeName string `json:"nodeName"`
	HostIP   string `json:"hostIP"`
	Port     int    `json:"port"`
	URL      string `json:"url"`
	Ready    bool   `json:"ready"`
}

// Pod 是 release 下的 Pod 视图。字段名以 models.HelmReleasePodDTO 的常见 JSON 为准，
// 缺字段不影响就绪合成（Pod Ready 只用于展示与日志发现，不参与 endpoint 决策）。
type Pod struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
	NodeName      string `json:"nodeName"`
	PodIP         string `json:"podIP"`
	HostIP        string `json:"hostIP"`
	Image         string `json:"image"`
	Ready         bool   `json:"ready"`
	RestartCount  int    `json:"restartCount"`
	CreationTime  string `json:"creationTimestamp"`
}

// PodList 是 `GET /api/open/helm/releases/{name}/pods` 的响应。
type PodList struct {
	Items []Pod `json:"items"`
}

// UninstallResult 是 DELETE 的响应。
type UninstallResult struct {
	Message        string   `json:"message"`
	ForcedPods     []string `json:"forcedPods"`
	SecretsDeleted int      `json:"secretsDeleted"`
	CleanupErrors  []string `json:"cleanupErrors"`
}

// QueueSnapshot 是队列水位（`GET /api/open/queue` + `/api/open/queue/podgroups`）。
// **没有位次**：Genet 只给计数（§9.1、§9.10 R2）。
type QueueSnapshot struct {
	Enabled   bool      `json:"enabled"`
	PoolType  string    `json:"poolType"`
	QueueName string    `json:"queueName"`
	Queue     *Queue    `json:"queue"`
	PodGroups []PodGroup `json:"podGroups"`
}

// Queue 是 `GET /api/open/queue` 里的 queue 对象（字段以 models/openapi_queue.go 为准）。
type Queue struct {
	Name             string            `json:"name"`
	State            string            `json:"state"`
	Running          int               `json:"running"`
	Pending          int               `json:"pending"`
	Inqueue          int               `json:"inqueue"`
	Capability       map[string]string `json:"capability"`
	Allocated        map[string]string `json:"allocated"`
	PhysicalCapacity map[string]string `json:"physicalCapacity"`
}

// PodGroup 是 `GET /api/open/queue/podgroups` 的一项。
type PodGroup struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Queue      string      `json:"queue"`
	Phase      string      `json:"phase"`
	MinMember  int         `json:"minMember"`
	Conditions []Condition `json:"conditions"`
}

// Condition 是 PodGroup 的 status.conditions[]（阻塞原因的原文来源）。
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// ---- runs.deploy 的冻结输入（Start 读它，不重新渲染）---------------------

// DeployManifest 是 `ports.DeployRequest.Manifest`（即 `runs.deploy`）里本层需要的字段。
// 形状与 `views/03-data-view.md §4.4` 一致；缺字段时用默认值兜底但不臆造 values。
type DeployManifest struct {
	DeployID     string     `json:"deploy_id"`
	Provider     string     `json:"provider"`
	Namespace    string     `json:"namespace"`
	ReleaseName  string     `json:"release_name"`
	PackageType  string     `json:"package_type"`
	Chart        string     `json:"chart"`
	ChartVersion string     `json:"chart_version"`
	Values       string     `json:"values"`
	ValuesDigest string     `json:"values_digest"`
	TemplatePath string     `json:"template_path"`
	Image        string     `json:"image"`
	ModelPath    string     `json:"model_path"`
	ModelName    string     `json:"model_name"`
	Set          []SetValue `json:"set"`
	NoWait       *bool      `json:"no_wait"`
	TimeoutSec   int        `json:"helm_timeout_sec"`
}