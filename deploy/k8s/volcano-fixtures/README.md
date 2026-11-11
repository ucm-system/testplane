# Volcano 演示夹具（**仅本地 kind，不要用于生产**）

用途：让 `GET /api/v1/queue-status` 的 `volcano` 块返回 `available:true` 的**真实 CRD 数据**，
用于 C-4.8 的验收与 UI 联调（design/impl/control-plane.md §C-4.8、design/views/06-physical-view.md §2.1）。

## 装了什么 / 没装什么

| | 内容 |
|---|---|
| 装了 | 12 个 Volcano CRD（取自官方 `installer/volcano-development.yaml` 的 CRD 子集）+ 2 个 `Queue`（`genet-shared` / `genet-exclusive`）+ 5 个 `PodGroup`（4 个在 `argus`、1 个故意放 `argus-mock` 用来验证 namespace 口径） |
| **没装** | `vc-scheduler` / `vc-controller-manager` / `vc-webhook-manager`。理由：webhook 会拦 Pod 创建、常驻组件会占用 kind 节点，而本轮验收只需要"真 apiserver + 真 CRD List"，不需要真调度 |

⚠ 因此夹具里的 `status.state` / `status.allocated` / `status.phase` / `status.conditions` 全是
`kubectl patch` 手工灌进去的**假水位**，与真实调度无关。它只能证明"平台读路径正确"，不能证明"集群调度正常"。

## 文件在哪

清单（夹具本体，**不入库**，`.gitignore` 里 `.data/` 整目录忽略）：

```text
.data/volcano-fixtures/
├─ crd-volcano.yaml                  # 12 个 CRD（从下面的 upstream 文件里提取）
├─ volcano-development.upstream.yaml # 官方原文件（对照/升级用；来源见下）
├─ queues.yaml                       # 2 个 Queue（spec.capability）
├─ podgroups.yaml                    # 5 个 PodGroup（spec.queue）
└─ apply-status.ps1                  # 把 state/allocated/phase/conditions 灌进去
```

upstream 来源（钉到日期的快照，不是每次自动拉最新）：

```text
https://raw.githubusercontent.com/volcano-sh/volcano/master/installer/volcano-development.yaml
```

## 一键装

```powershell
# 1) CRD（12 个；只是声明 apiGroups/resources，集群里没有 Volcano 也能 apply）
kubectl apply -f .data/volcano-fixtures/crd-volcano.yaml

# 2) 队列 + PodGroup
kubectl apply -f .data/volcano-fixtures/queues.yaml
kubectl apply -f .data/volcano-fixtures/podgroups.yaml

# 3) 灌 status（Queue 有 status 子资源；PodGroup 的 CRD **没有** status 子资源，直接 merge patch）
pwsh .data/volcano-fixtures/apply-status.ps1
```

装完 `GET /api/v1/queue-status` 的期望块（`AG_CLUSTER_NAME=cluster-01`）。
**这只是形态参考，不要当断言基线**：`longest_wait_seconds` 等数值会随夹具创建时间变化（见下方说明）。

```json
"volcano": {
  "available": true,
  "detail": null,
  "queues": [
    {"name":"cluster-01/genet-exclusive","state":"Closed",
     "capability":{"cpu":"128","huawei.com/Ascend910":"16"},
     "allocated":{"huawei.com/Ascend910":"0"},
     "pending_jobs":1,"longest_wait_seconds":42,
     "reason":"queue genet-exclusive is not open"},
    {"name":"cluster-01/genet-shared","state":"Open",
     "capability":{"cpu":"64","memory":"128Gi","nvidia.com/gpu":"8"},
     "allocated":{"cpu":"16","nvidia.com/gpu":"3"},
     "pending_jobs":2,"longest_wait_seconds":42,
     "reason":"0/1 nodes are available: 1 insufficient nvidia.com/gpu"}
  ]
}
```

哪些字段稳定、哪些会变：

| 字段 | 是否稳定 | 说明 |
|---|---|---|
| `available` / `detail` | 稳定 | `true` / `null`（只要 CRD 装好且 RBAC 够） |
| `name` | 稳定 | `"<AG_CLUSTER_NAME>/<queue 名>"`，默认 `cluster-01/...` |
| `state` / `capability` / `allocated` / `pending_jobs` | 稳定 | 由 `queues.yaml` + `apply-status.ps1` + `podgroups.yaml` 决定（夹具不动就不变） |
| `reason` | 稳定 | condition 原文，两条固定：`genet-exclusive` → `queue genet-exclusive is not open`；`genet-shared` → `0/1 nodes are available: 1 insufficient nvidia.com/gpu`（取等得最久那条的 `conditions[-1].message`） |
| **`longest_wait_seconds`** | **会变** | `now - min(该队列下 phase ∈ {Pending,Inqueue} 的 PodGroup.metadata.creationTimestamp)`，也就是"夹具建了多久"。上面写的 `42` 只是拍了一次的示例：刚 apply 完是个位数，过 10 分钟就是 600+，**每次请求都在涨**。要断言只能断言"`> 0` 且单调不减"，不能断言等于某个数 |

`pending_jobs=2`（`genet-shared` 下 2 条 `Pending`；第 3 条是 `Inqueue`，**不计**）与
`argus-mock` 那条 Pending **不出现**，正是 `AG_VOLCANO_NAMESPACES`（默认 profile 的 namespace）
这条口径的证据。

## 一键删（恢复"集群没装 Volcano"）

```powershell
kubectl delete queue genet-shared genet-exclusive
kubectl delete podgroup -n argus tp-volcano-pending-1 tp-volcano-pending-2 tp-volcano-inqueue tp-volcano-exclusive-1
kubectl delete podgroup -n argus-mock tp-volcano-other-ns
kubectl delete -f .data/volcano-fixtures/crd-volcano.yaml
```

删完再请求 `/queue-status` 应回到：

```json
"volcano":{"available":false,"detail":"crd_missing","queues":[]}
```

（这就是 B 计划：没有 CRD 时 `IsNotFound` → `crd_missing`，**不会**退化成"0 排队"。）

## 与 RBAC 的关系

`deploy/k8s/rbac.yaml` 里的 `ClusterRole argus-volcano-view`（`queues`/`podgroups`：`get,list`，
**不给 watch、不给 status 写**）就是读到本夹具数据所需的全部权限；Queue 是 cluster-scoped，
namespace 级 `Role` 绑不到。反过来，**没装 CRD 时这个 ClusterRole 照样能 apply**——ClusterRole
只声明 `apiGroups`/`resources`，不做 discovery，只有真正 List 时才返回 `IsNotFound`（→ `crd_missing`）
或 `IsForbidden`（→ `rbac_forbidden`）。
