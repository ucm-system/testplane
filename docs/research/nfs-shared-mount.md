# NFS 挂进 Tester Pod：三种形态、restricted 兼容性、kind 供给与平台落点

对应 wayfinder 研究票 [#69](https://github.com/ucm-system/testplane/issues/69)，父地图
[#66「Tester 统一外部存储挂载（NFS 读模型权重）」](https://github.com/ucm-system/testplane/issues/66)。

**一句话结论**：**只有「PVC 路线」在现有 `restricted:latest` + uid 10001 下开箱可用**；
Pod spec 内联 `volumes[].nfs` 会被 PodSecurity 直接拒绝，不是"能不能用"而是"根本建不出 Pod"。
NFS 侧的权限约束不是 `root_squash`（它只压 uid 0），而是**文件 mode**：`10001` 读 0644 文件没问题，
读 root 自有的 0600 文件一定失败。

---

## 0. 方法与证据分级

| 标记 | 含义 |
|---|---|
| **实测** | 在本机 kind 集群真跑，附命令与输出片段（临时清单在 `.data/nfs-research/`，不入库） |
| **读源码** | 本仓库或 K8s 上游源码，附 `文件:行号` |
| **读文档** | 官方文档，附链接 + 章节锚点 |

实测环境（2026-11-06）：

```
$ kubectl config current-context
kind-testplane
$ kubectl get nodes
NAME                      STATUS   ROLES           AGE   VERSION
testplane-control-plane   Ready    control-plane   31h   v1.33.1
# 节点 OS：Debian GNU/Linux 12 (bookworm)；运行时 containerd://2.1.1
# 宿主内核：6.18.54.1-microsoft-standard-WSL2（WSL2 上的 Docker Desktop，kind 节点共享该内核）
$ kubectl get ns testplane -o jsonpath='{.metadata.labels}'
{"pod-security.kubernetes.io/audit":"restricted","pod-security.kubernetes.io/enforce":"restricted",
 "pod-security.kubernetes.io/warn":"restricted", ...}
```

一手来源清单见 §8。引用锚点（`R-N:行号` = 本文件行号）见 §9。

---

## 1. 结论先行

1. **形态①（内联 `volumes[].nfs`）在 `testplane` ns 不可用**。`restricted:latest` 的卷类型白名单
   是封闭集合，不含 `nfs`；API Server 在准入阶段就拒绝（**实测**，§2.1）。要让它能用，只能放宽
   namespace 的 PSA —— 那是把整个 Tester 的安全基线降级，不可接受。
2. **形态②（预建 PV `spec.nfs` + PVC，Pod 引 `claimName`）开箱可用**。Pod 侧卷类型是
   `persistentVolumeClaim`，在白名单内；uid 10001 / 只读根 / drop ALL / RuntimeDefault 全部实测通过（§2.2、§3）。
3. **形态③（CSI，如 `csi-driver-nfs`）准入上等价于②**（同样是 PVC），代价是要多部署一套驱动；
   它对 `fsGroup` 的处理与②**不同**，在 `root_squash` 导出上更容易炸（§2.3、§3.3）。
4. **权限模型的关键事实**：`root_squash` 只压 uid 0，**不影响 uid 10001**。10001 能不能读，
   完全由「文件所有者 + mode」决定（**实测** §3.2）：

   | 文件 | 属主/mode | uid 10001 能否读 |
   |---|---|---|
   | `root0644.txt` | `0:0 0644` | ✅ 能 |
   | `root0600.txt` | `0:0 0600` | ❌ `Permission denied` |
   | `tester10001.txt` | `10001:10001 0600` | ✅ 能 |

   → **导出模型权重的运维规则只有一条**：目录 `0755`、文件 `0644`（或整体 `chown -R 10001:10001`）。
5. **`fsGroup` 对普通 NFS 卷无效**（**实测 + 读源码**）：`render.go` 注入的 `fsGroup: 10001`
   在 NFS mount 上不产生任何 chown/chgrp，导出目录属主始终是 `0:0`（§3.3）。
6. **换 NFS 地址的代价取决于形态**：②要重建 PV（`spec.persistentvolumesource` 不可变，**实测**），
   ①要改 `tester.yaml` 并发新版本，③要新建 StorageClass + 新 PVC（存量 PV 已固化参数）。
7. **kind 上起 NFS server 可行**，但有两道真实前提：节点内**有** `mount.nfs`（kindest/node 自带），
   宿主内核**要** `nfsd` 模块（容器内 `modprobe` 不可用，实测 §4）。

---

## 2. 三种形态逐条对比

| 维度 | ① Pod 内联 `volumes[].nfs` | ② 预建 PV(`spec.nfs`) + PVC | ③ CSI（`csi-driver-nfs`） |
|---|---|---|---|
| **能否在 `testplane` ns 建出 Pod** | ❌ **被 PSA 拒绝**（实测） | ✅ 能（实测） | ✅ 能（Pod 侧同为 PVC） |
| 先决条件 | 无（不需要 PVC），但要求 namespace 放开 PSA | 集群管理员造 PV；PVC 绑定 | 部署 CSI 驱动（controller + node DaemonSet）+ StorageClass |
| 谁维护生命周期 | kubelet（Pod 生命周期内挂/卸），无 API 对象 | PV 由管理员；`persistentVolumeReclaimPolicy` ∈ Retain/Recycle/Delete | 动态供给：CSI controller 建/删 NFS 子目录（`onDelete: delete\|retain\|archive`） |
| 能否 `subPath` | ✅（`volumeMount.subPath`，K8s 层，与卷类型无关） | ✅ **实测** | ✅ 同上；另有驱动级 `subDir` 参数 |
| 能否只读 | ✅ `nfs.readOnly` + `volumeMount.readOnly` | ✅ 两层：`pv.spec.nfs.readOnly` + `volumeMount.readOnly`（实测 `/proc/mounts` 带 `ro`） | ✅ `accessModes: ReadOnlyMany` + `volumeMount.readOnly` |
| 能否设 `mountOptions` | ❌ **不能**（官方文档明文） | ✅（`spec.mountOptions`，实测可变） | ✅（StorageClass `mountOptions`） |
| 换 NFS 地址 | 改 `tester.yaml` → 发新 Tester 版本 → Job spec-hash 变 | **必须重建 PV**（`persistentvolumesource` 不可变，实测）→ PVC 也要重建 | 改 StorageClass 的 `server` 只影响**新** PVC；存量 PV 的 `volumeAttributes` 已固化 |
| accessModes | 卷本身无 accessMode 概念 | `RWO / ROX / RWX` 都支持 | 由驱动声明 |

### 2.1 形态①：内联 `volumes[].nfs`

官方定义见 [Volumes § nfs](https://kubernetes.io/docs/concepts/storage/volumes/#nfs)（**读文档**），
写法就是 `spec.volumes[].nfs: {server, path, readOnly}`。

官方同段还有一句关键限制（**读文档**，同上链接）：

> Also note that you can't specify NFS mount options in a Pod spec. You can either set mount options
> server-side or use `/etc/nfsmount.conf`. You can also mount NFS volumes via PersistentVolumes,
> which do allow you to set mount options.

**本平台不可用的直接证据（实测）**：把与 `render.go` 渲染结果同形的 Pod（uid 10001、`readOnlyRootFilesystem`、
drop ALL、`RuntimeDefault`）改成内联 NFS，投进与 `testplane` 标签完全一致的 namespace：

```console
$ kubectl apply -f .data/nfs-research/30-inline-nfs-pod.yaml
namespace/tp-nfs-restricted created
Error from server (Forbidden): error when creating "...30-inline-nfs-pod.yaml":
  pods "inline-nfs" is forbidden: violates PodSecurity "restricted:latest":
  restricted volume types (volume "models" uses restricted volume type "nfs")
```

对照官方规范（**读文档**，[Pod Security Standards § Restricted → Volume Types](https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted)）：
restricted 只允许 `configMap` / `csi` / `downwardAPI` / `emptyDir` / `ephemeral` /
`persistentVolumeClaim` / `projected` / `secret` 八种。**`nfs` 不在其中**，`hostPath` 在 baseline 就已被禁。

> 推论：`testplane` ns 的 PSA 是设计上的硬约束（`render.go` 的 securityContext 与它配套），
> 所以形态①不是"以后优化"，而是**结构上被排除**。

### 2.2 形态②：PV `spec.nfs` + PVC（推荐）

官方定义：[Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#types-of-persistent-volumes)
里 `nfs` 是支持的内置类型；同一页明确 `NFS ✓ RWO ✓ ROX ✓ RWX`。

同页另一条与 kind 直接相关（**读文档**，Persistent Volumes § Types of Persistent Volumes）：

> ... of type NFS and the helper program `/sbin/mount.nfs` is required to support the mounting of NFS filesystems.

以及 `mountOptions` 的支持范围（**读文档**，Persistent Volumes § Mount Options）：

> The following volume types support mount options: `csi` (including CSI migrated volume types)、`iscsi`、`nfs`

**实测通过的清单**（`.data/nfs-research/40-pv-pvc-pod.yaml`）：PV `spec.nfs{server, path, readOnly}` +
PVC（`accessModes: [ReadOnlyMany]`，`storageClassName: nfs-static` 静态绑定，`volumeName` 预绑）+
Pod 用 `persistentVolumeClaim.claimName`。Pod 的安全上下文与 `render.go:182-213` 逐字段一致。

```console
$ kubectl -n tp-nfs-restricted exec pvc-nfs -- sh -c '...'
uid=10001 gid=10001 groups=10001
-rw------- 1 0     0     19 root0600.txt
-rw-r--r-- 1 0     0     26 root0644.txt
-rw------- 1 10001 10001 15 tester10001.txt
root-owned-world-readable                     # 0644 读成功
cat: .../root0600.txt: Permission denied      # 0600 被拒
uid10001-owned                                # 10001 自有 0600 读成功
touch: /models/models/write-test: Read-only file system   # 只读生效
$ grep nfs /proc/mounts
172.18.0.6:/ /models nfs4 ro,relatime,vers=4.2,rsize=1048576,wsize=1048576,namlen=255,
  hard,fatal_neterrors=ENETDOWN:ENETUNREACH,proto=tcp,timeo=600,retrans=2,sec=sys,...
```

要点：
- `ro` 由 **volumeMount 的 `readOnly`** 落到 `/proc/mounts`；写操作报 `Read-only file system`（内核态拒绝，不是 EACCES）。
- `subPath` 完全可用（**实测**，`.data/nfs-research/60-subpath.yaml`）：同一 PVC 挂两次，
  `subPath: models` → `/weights`，`subPath: models/Qwen3-0.6B` → `/w2`，都正确。
- **PV 的 `spec.persistentvolumesource` 创建后不可变**（**实测**）：

  ```console
  $ kubectl patch pv nfs-models-pv --type=merge -p '{"spec":{"nfs":{"server":"172.18.0.7"}}}'
  The PersistentVolume "nfs-models-pv" is invalid: spec.persistentvolumesource: Forbidden:
    spec.persistentvolumesource is immutable after creation
  ```
  并对照：`spec.mountOptions` **可以**改（`patched`）。但改 mountOptions 只对**之后新挂载**的 Pod 生效。
- 因此"换 NFS 地址"= 删 Pod → 删 PVC → 删 PV → 重建。**PVC 的 `volumeName` 预绑还能让新 PVC 名称不变**，
  这样平台侧配置（`tester.yaml` 的 `source`）不用动 —— 这是②相对③的一个隐藏优点。

### 2.3 形态③：CSI（`csi-driver-nfs`）

一手来源：[csi-driver-nfs `docs/driver-parameters.md`](https://github.com/kubernetes-csi/csi-driver-nfs/blob/master/docs/driver-parameters.md)（**读文档**）。

- 先决条件（原文）："This driver requires existing and already configured NFSv3 or NFSv4 server"，
  再 + 驱动部署 + StorageClass。
- 动态供给：controller 在 share 下 `MkdirAll(..., 0777)` 建子目录；`subDir` 支持
  `${pvc.metadata.name}` / `${pv.metadata.name}` 等占位符。
- 静态供给：PV 用 `spec.csi{driver: nfs.csi.k8s.io, volumeHandle: "server#share#subdir", volumeAttributes{server,share}}`。
- 只读：`volumeAttributes` 的 `uid`/`gid` 在**只读 publish 时被跳过**（原文："read-only publishes skip `chown`"）。
- `subPath`：K8s 层照旧可用；驱动另有 `subDir`。
- **未实测**：本机 `git`/`kubectl apply -k` 到 github.com 被网络阻断
  （`Failed to connect to github.com port 443 after 21115 ms`），驱动装不上。结论来自官方文档，
  其与②的差异用**机制级实测**补齐（§3.3 的 chown EPERM）。

---

## 3. 与平台 Pod 约束的兼容性（最关键一节）

平台现状（**读源码**，`server/internal/executor/render.go:182-213`）：Pod 级 `runAsNonRoot`+
`runAsUser/RunAsGroup/FSGroup = 10001`+`seccompProfile: RuntimeDefault`；容器级
`readOnlyRootFilesystem: true`、`allowPrivilegeEscalation: false`、`capabilities.drop: [ALL]`。

### 3.1 唯一会"卡住"的约束是 PodSecurity 的卷白名单

- `readOnlyRootFilesystem` —— 不影响，NFS 挂载点本来就是独立 mount，不写容器根。
- `drop ALL` / `runAsNonRoot` / `seccomp` —— **不影响**。NFS 的 `mount(2)` 由 **kubelet 在节点上**执行
  （实测 `/proc/mounts` 里 `clientaddr=172.18.0.6` 是**节点 IP**，不是 Pod IP），
  容器进程根本不参与挂载，也就用不到 `CAP_SYS_ADMIN`。
- PodSecurity `restricted:latest` —— **只有卷类型这一条会卡**，见 §2.1。

> 补充：kind 单节点里恰好 `clientaddr == 节点 IP`，容易让人误会"是容器挂的"。换真实多节点集群，
> 这个字段会是实际承载 Pod 的节点 IP；结论不变。

### 3.2 uid 10001 与 `root_squash` 的真实关系（**实测**）

常见说法是"`root_squash` 会让非 root 读不了"。**这是错的**。`root_squash` 只把**客户端的 uid 0**
映射成 `65534`(nobody)；任何非 0 uid（这里 10001）原样透传（对应导出选项 `no_all_squash`）。
所以：

1. **属主显示不变**。`root_squash` 下客户端 `ls -ln` 仍然看到 root 自有文件是 `0:0`
   —— squash 只作用于**访问判定**，不改 inode 上报的 uid。
2. **10001 的可读性只由 mode 决定**（§1 第 4 条的表格）。
3. **被 squash 掉的是"节点 root"**，这才是运维会撞到的地方：

```console
# 导出：/nfsshare *(rw,fsid=0,async,no_subtree_check,no_auth_nlm,insecure)   ← 默认 root_squash
$ mount -t nfs4 -o ro 172.18.0.6:/ /mnt/squash-probe
$ ls -ln /mnt/squash-probe/models
-rw------- 1     0     0 19 root0600.txt      # 属主仍显示 0
$ cat /mnt/squash-probe/models/root0600.txt
cat: ...: Permission denied                   # 节点 root 已被压成 65534，读不了
```

4. **`root_squash` 下 root 连 mkdir 都可能失败**（当导出根是 `0755 root:root`）：

```console
$ mkdir /mnt/squash-probe/chowntest
mkdir: cannot create directory '/mnt/squash-probe/chowntest': Permission denied
# 把导出根放开到 0777 后可以 mkdir（dir 属主是 65534），但 chown 仍然失败：
$ chown 10001:10001 /mnt/squash-probe/chowntest
chown: changing ownership of '...': Operation not permitted
$ chgrp 10001 /mnt/squash-probe/chowntest
chgrp: changing group of '...': Operation not permitted
```

第 4 条正是 §3.3 里 `fsGroup` 会炸的机制。

5. **`all_squash,anonuid=10001` 不能救 root 自有的 0600 文件**（**实测**）：`all_squash` 把所有客户端
   uid（含 root）都映射成 `anonuid`。此时 root 读 `tester10001.txt`（属主 10001）成功，
   读 `root0600.txt`（属主 0）仍 `Permission denied` —— 因为映射后的 uid 是 10001，不是 0。
   → `all_squash` 适合"多租户只给一个 uid 用"，不适合"共享已有 root 自有的模型目录"。

### 3.3 `fsGroup` 对 NFS 到底有没有效果（**实测 + 读源码**）

**普通 NFS 卷：没有效果。**

实测：Pod 的 `securityContext.fsGroup: 10001`，挂载后导出目录与挂载点的属主/属组**始终是 `0:0`**
（`ls -ldn /var/lib/tp-nfs /var/lib/tp-nfs/models` 与 `ls -ldn /mnt/squash-probe` 都是 `0 0`）。

源码对得上（**读源码**，K8s `pkg/volume/nfs/nfs.go`，master）：

```go
func (nfsMounter *nfsMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:       nfsMounter.readOnly,
		Managed:        false,
		SELinuxRelabel: false,
	}
}

func (nfsMounter *nfsMounter) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	...
	err = nfsMounter.mounter.MountSensitiveWithoutSystemd(source, dir, "nfs", mountOptions, nil)
```

`SetUpAt` 收了 `mounterArgs`（里面就有 `FsGroup`）却**从头到尾没用**——所以 kubelet 即使传了 fsGroup，
NFS 插件也不会做 chown/chgrp。同一文件里还有一处注释直接承认 root_squash 的存在：

```go
// Use extensiveMountPointCheck to consult /proc/mounts. We can't use faster
// IsLikelyNotMountPoint (lstat()), since there may be root_squash on the
// NFS server and kubelet may not be able to do lstat/stat() there.
```

**CSI NFS（③）：有效果，而且会和 `root_squash` 冲突。** 驱动文档原文（**读文档**，
[driver-parameters.md](https://github.com/kubernetes-csi/csi-driver-nfs/blob/master/docs/driver-parameters.md)）：

> This driver ships `fsGroupPolicy: File`. After `NodePublishVolume` returns, kubelet applies
> `pod.spec.securityContext.fsGroup` when it is set: kubelet runs `chown(-1, fsGroup)` recursively:
> **UID is left unchanged, GID is replaced**, and the setgid bit is set
>
> Prerequisites: ... the NFS export must allow the kubelet-issued `chown`/`chgrp` on the mount root
> (i.e. `no_root_squash`, or a `squash_uid` that owns the share). **Root-squashed exports may reject the
> ownership change and surface as a mount failure instead of a write failure.**

"reject the ownership change" 就是 §3.2 第 4 条实测到的 `Operation not permitted`。
→ **结论：如果哪天为了③把导出改成 `root_squash`，Tester Pod 会从"能跑"变成"挂不上"**，
而②在没有 CSF 的情况下静默忽略 fsGroup，反而不会因此失败。

### 3.4 导出参数该怎么给

给 **只读模型权重** 的场景，最小且够用的导出（**实测**通过，`itsthenetwork/nfs-server-alpine` 可配置的范围）：

```
/nfsshare  <client-cidr>(ro,fsid=0,sync,no_subtree_check,insecure,no_root_squash)
```

- `ro` —— 服务端兜底只读，即使 Pod 忘了 `readOnly: true`。
- `sync` —— 本场景只读，`async`/`sync` 无差别；若是**产物目录**（可写）建议 `sync`（本项目 `04-process-view.md:253`
  已经把"NFS 抖动导致产物写不进"列为风险）。
- `no_subtree_check` —— 现代默认，减少 rename 后的句柄失效。
- `no_root_squash` —— **只有**在需要"root 建目录/改属主"（③的 fsGroup、或管理员在客户端侧 chown）时才需要。
  纯只读消费可以保留 `root_squash`（更安全），因为 uid 10001 不受影响。
- `anonuid`/`anongid` —— 只在用 `all_squash` 时配对出现；见 §3.2 第 5 条，它靠不住。
- `insecure` —— 客户端源端口 >1024 时需要（容器里发起挂载时常见；本次实测的导出带它）。

### 3.5 `readOnlyRootFilesystem` 与 NFS 只读的相互作用

- `readOnlyRootFilesystem: true` 让 `/` 只读，`/tmp` 由平台挂 `emptyDir`（`render.go:62-63`）。
  NFS 挂载点是额外 mount，不受影响（**实测**：busybox 在只读根下正常读 NFS）。
- **`HOME=/tmp`** 已经在 `render.go:422` 注入，所以只读根 + 只读 NFS 也不会让 Python/torch
  因为"写不了 `~/.cache`"而炸——这点对本场景很重要（aisbench 会写 tokenizer 缓存）。
- Pod 内**不能**改 NFS 挂载的属主（没有 `CAP_CHOWN`，`drop ALL`）——这也解释了为什么"让文件可读"
  必须在导出端一次做对。

---

## 4. kind 上提供 NFS

### 4.1 节点现状（**实测**，与"kind 节点缺 `nfs-common`"的常见假设相反）

```console
$ docker exec testplane-control-plane sh -c 'which mount.nfs; ls -l /sbin/mount.nfs'
/usr/sbin/mount.nfs
-rwsr-xr-x 1 root root 130056 Dec 11  2024 /sbin/mount.nfs
$ docker exec testplane-control-plane sh -c 'cat /proc/filesystems | grep -i nfs'
nodev	nfs
nodev	nfs4
```

- `kindest/node:v1.33.1` 基于 Debian 12，**自带 `nfs-common`**，`mount.nfs` 位于 `/usr/sbin` 且带 setuid 位。
- 内核侧 `nfs`/`nfs4` **已就绪**（kind 节点共享宿主 WSL2 内核，宿主已加载这两个模块）。
- **但 `nfsd`（服务端）没有**：`/proc/fs/nfsd/versions` 不存在、`lsmod` 里没有 `nfsd`。

### 4.2 起 NFS server 的真实前提：宿主内核加载 `nfsd`

kind 节点容器内**没有 `/lib/modules`**，`modprobe` 直接失败：

```console
$ docker run --rm --privileged alpine:3.20 sh -c 'modprobe nfsd'
modprobe: can't change directory to '/lib/modules': No such file or directory
```

正确做法是**在宿主（Docker Desktop 的 WSL 发行版）加载模块**，加载后容器立刻可见：

```console
$ wsl -d docker-desktop -e sh -c 'ls /lib/modules/6.18.54.1-microsoft-standard-WSL2/kernel/fs/nfsd/'
nfsd.ko
$ wsl -d docker-desktop -e sh -c 'modprobe nfsd; grep nfsd /proc/modules'
nfsd 753664 0 - Live 0xffffffffc085d000
```

之后 `kubectl` 侧一切正常。注意内核 6.18 的 `nfsd` 控制面已迁到 netlink，
`/proc/fs/nfsd/versions` **仍然不存在**，这是正常的，不代表模块没加载（`/proc/modules` 才是判据）。

容器里起 nfsd 时内核会打三条无害告警（**实测** `dmesg`）：

```
NFSD: attempt to initialize umh client tracking in a container ignored.
NFSD: Unable to initialize client recovery tracking! (-22)
NFSD: Is nfsdcld running? If not, enable CONFIG_NFSD_LEGACY_CLIENT_TRACKING.
```

### 4.3 可复现步骤（**实测走通的路径**）

清单在 [`.data/nfs-research/10-server.yaml`](../../.data/nfs-research/10-server.yaml)（临时产物，不入库）。要点：

```yaml
# namespace 不能开 restricted：nfsd 需要 privileged
apiVersion: v1
kind: Pod
metadata: {name: nfs-srv, namespace: tp-nfs-srv}
spec:
  containers:
    - name: nfs
      image: itsthenetwork/nfs-server-alpine:12
      securityContext: {privileged: true}
      env:
        - {name: SHARED_DIRECTORY, value: /nfsshare}
        - {name: PERMITTED, value: "*"}
      ports: [{containerPort: 2049, hostPort: 2049}]
      volumeMounts: [{name: share, mountPath: /nfsshare}]
  volumes:
    - name: share
      hostPath: {path: /var/lib/tp-nfs, type: DirectoryOrCreate}
```

```console
$ kubectl apply -f .data/nfs-research/10-server.yaml
namespace/tp-nfs-srv created
pod/nfs-srv created
$ kubectl -n tp-nfs-srv wait --for=condition=Ready pod/nfs-srv --timeout=120s
pod/nfs-srv condition met
$ kubectl -n tp-nfs-srv exec nfs-srv -- exportfs -v
/nfsshare  <world>(async,wdelay,hide,no_subtree_check,insecure_locks,fsid=0,sec=sys,rw,insecure,no_root_squash,no_all_squash)
```

**三个必须知道的细节**：

1. **`hostPort: 2049`** 让节点 IP 能直接访问（`172.18.0.6:2049`）。NFSv4 只用 2049 一个端口，
   rpcbind/mountd 可以不开放（该镜像已禁用 v2/v3）。
2. **`fsid=0`** 让 `/nfsshare` 成为 NFSv4 伪根，所以 PV 的 `path: /` 就**是** `/nfsshare`；
   `path: /models` 是 `/nfsshare/models`。这一点直接决定 PV 里 `path` 怎么写，写错就是
   `No such file or directory`（§6.1）。
3. **该镜像默认导出带 `no_root_squash`**（见上面的 `exportfs -v`，也见
   `/etc/exports` 模板 `/nfsshare {{PERMITTED}}({{READ_ONLY}},fsid=0,{{SYNC}},no_subtree_check,no_auth_nlm,insecure,no_root_squash)`）。
   **这和真实 NFS 服务器的默认相反**（`exportfs` 缺省是 `root_squash`）。
   用这个镜像做验证会**掩盖** §3.2/§3.3 的坑，必须手工 `sed -i 's/,no_root_squash//' /etc/exports && exportfs -ra`
   再测一遍。本文的 squash 结论都是翻转后测的。

### 4.4 节点缺 `mount.nfs` 会怎样（**实测**，用改名复现，测完已恢复）

官方文档明确要求这个 helper（[Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#types-of-persistent-volumes)：
"the helper program `/sbin/mount.nfs` is required"）。真缺了以后的报错很**不直观**：

```console
$ mv /usr/sbin/mount.nfs /usr/sbin/mount.nfs.bak
$ mount -t nfs -o ro 172.18.0.6:/ /mnt/nopkg
mount: /mnt/nopkg: bad option; for several filesystems (e.g. nfs, cifs) you might need
       a /sbin/mount.<type> helper program.
       dmesg(1) may have more information after failed mount system call.
# rc=32
```

`exit status 32` + `bad option` 会被 kubelet 原样塞进 `FailedMount` 事件里，
**看起来像挂载参数写错，实际是节点没装 `nfs-common`**。排查时先 `which mount.nfs`。

三种解法（按代价排序）：

1. **换 kind 节点镜像** —— `kindest/node` 已自带，正常不用管（本次实测 `v1.33.1` 就有）。
   换成精简节点镜像才需要处理。
2. **在节点容器里装** —— `docker exec testplane-control-plane apt-get install -y nfs-common`。
   临时、可复现，但 `kind delete cluster` 后丢失。
3. **用 CSI** —— 驱动 node 插件是 privileged DaemonSet，镜像自带 `mount.nfs`，不依赖节点包。
   代价是整套驱动（§2.3）。

---

## 5. 平台渲染面要改什么

### 5.1 走 PVC 路线（②/③）：**零代码改动**

现状（**读源码**）：

- `domain.MountSpec{Name, MountPath, ReadOnly, Source}`（`server/internal/domain/types.go:206-212`），
  `Source` 就是 PVC 名。
- `buildMounts()`（`server/internal/executor/render.go:513-589`）对每条 `spec.Mounts` 只产
  `PersistentVolumeClaimVolumeSource`；`Source` 为空时回落 `spec.StoragePVC`（`render.go:573-576`）。
- `ports.JobSpec.Mounts []domain.MountSpec`（`server/internal/ports/ports.go:59`）只是透传，
  `registry.go:568-576` 从 `tester.yaml` 解析。

**所以把 NFS 当成"一个预先存在的 PVC"就完事**：运维侧建 PV+PVC，`tester.yaml` 写

```yaml
mounts:
  - {name: models, mount_path: /models, read_only: true, source: nfs-models}
```

平台一行代码都不用改。**这也解释了为什么这条路线是唯一推荐的**——它复用已经实测通过的渲染路径。

chart 侧若要"一个开关搞定"，最小改动是：

- `deploy/charts/testplane/templates/` 加一个可选 PV 模板（`spec.nfs{server, path}`）+ 可选 PVC；
- `values.yaml` 加 `modelsShare: {enabled, server, path, claimName, readOnly}` 一类的键；
- **注意已有伏笔**：`values.yaml:223-224` 已经有 `networkpolicy.storagePort: 2049` / `storageCIDR`，
  即出站到 NFS 端口的 NetworkPolicy 早就留好了；`values.yaml:134` 的
  `persistence.existingClaim`（注释直接写"NFS/CSI"）是现成的挂点。
  `deploy/k8s/storage.yaml` 的注释也写了"多节点/生产必须换 NFS CSI（06 §5.5、W-9）"。

### 5.2 真要加内联 nfs 分支，要动哪些（**不推荐**，仅列出代价）

1. `domain.MountSpec`（`types.go:206-212`）要扩成可判别联合：加 `Kind`/`NFS{Server, Path}`。
   连带改：`api/dto.go:123`、`api/openapi.yaml` 的 Tester schema（`server/api/openapi.yaml:4456` 附近）、
   `orchestrator/repos.go:82`、`registry.go:568-576`。
2. `ports.JobSpec`（`ports.go:59`）可以不动（`[]domain.MountSpec` 透传），但若要"**所有** Tester 统一挂"，
   更合适的不是让每个 `tester.yaml` 声明，而是在 `JobSpec` 加平台级字段（如 `SharedMounts`），
   由 `cmd/testplaned/wire.go` / `orchestrator/controller.go` 注入。
3. `render.go buildMounts()` 加分支，且要处理两件现在**没做**的事：
   - **volume 名去重**：现在只查 `mount_path` 冲突（`render.go:552-564` 的 `used` map），
     `sanitizeLabel(name)` 之后没有查重，两个 `mounts` 可能产同名 volume（K8s 会拒绝）。
   - `used` map 已含 `/workspace/input`（`render.go:552`）。
4. **然后再被 PSA 拒绝**（§2.1）。除非同时放宽 namespace（不可接受），否则第 1–3 步都是白做。

### 5.3 顺带发现的一处校验不一致（**读源码**）

- `registry.go:81-85` 的保留路径是 `/workspace/output`、`/tmp`、**`/tester`**；
- `render.go:552` 的 `used` map 是 `/workspace/output`、`/tmp`、**`/workspace/input`**。

两边不是同一个集合：`/tester` 在提交期被拒、渲染期却允许；`/workspace/input` 反之。
真要动挂载校验（票 #69 的落点之一），这两处应合并成一份共用常量。

---

## 6. 失败可见性

### 6.1 Pod 侧的失败形状（**实测**）

**A. 服务器可达、导出路径不存在**（PV `path: /no-such-dir`）：

```console
$ kubectl -n tp-nfs-restricted get events --field-selector involvedObject.name=pod-bad-path
FailedMount  MountVolume.SetUp failed for volume "nfs-bad-path-pv" : mount failed: exit status 32
Mounting command: mount
Mounting arguments: -t nfs -o ro 172.18.0.6:/no-such-dir /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~nfs/nfs-bad-path-pv
Output: mount.nfs: mounting 172.18.0.6:/no-such-dir failed, reason given by server: No such file or directory
```

Pod 停在 `ContainerCreating`，**Job 不失败**——这正是本平台最大的观感问题。

**B. 服务器不可达**（PV `server: 172.18.0.1`，端口拒绝）：

```console
FailedMount  MountVolume.SetUp failed for volume "nfs-unreachable-pv" : mount failed: exit status 32
Mounting arguments: -t nfs -o ro 172.18.0.1:/ ...
Output: mount.nfs: Connection refused
```

**延迟实测**：Pod 创建后 **约 80 秒**才出现 `FailedMount`；期间节点上有卡住的
`mount -t nfs ...` 进程（`ps` 可见），Pod 毫无进展。

> 如果对端是**丢包**而不是拒绝连（机房防火墙常见），NFS `hard` 挂载会一直重试，
> 事件可能长时间不出现。`/proc/mounts` 里能看到 `hard` 是默认值（本次实测 `hard,timeo=600,retrans=2`）。
> **这条对平台的含义**：靠"运行期看 FailedMount"来报错，用户可能干等几分钟。

**C. 节点缺 helper**（§4.4）：`Output` 变成 `bad option ... you might need a /sbin/mount.<type> helper program`。

### 6.2 平台现在是谁在报 `MOUNT_UNAVAILABLE`（**读源码**）

- 只在 **运行期**：`server/internal/executor/observe.go:505-518`，扫 Pod 的 `PodScheduled=False` 条件，
  message 命中 `mountFailureHints`（`observe.go:51`：`volume`/`mount`/`FailedMount`/`FailedAttachVolume`/`MountVolume`）
  就置 `CodeMountUnavailable`，然后在 `observe.go:253-258` 转成 `PhaseFailed` + `"MOUNT_UNAVAILABLE: <原文>"`。
- **dry-run 阶段没有任何探测**。`isKnownCode`（`observe.go:422-430`）只是把码认出来，不产生码。
- 设计文档这边有**两套说法**，实现只落到一套：
  - `design/impl/execution.md:171-172` 说"渲染期逐项检查…任一失败 → `MOUNT_UNAVAILABLE`，**必须在创建 Job 之前暴露**"；
  - `design/ARCHITECTURE.md:1890` 的 **D-60** 说"`MOUNT_UNAVAILABLE` 是**运行期**观测，**不是**提交期校验——提交期无法可靠判断 Profile/StorageClass 能否供给"。
  实现（`observe.go`）站 D-60 这边。**§6.1 的实测支持 D-60**：路径不存在/端口拒绝只有在 kubelet 真去
  mount 的时候才知道，提交期无法判断。

**能不能在提交期探到？** 可以做一个**弱**探测，但不能替代运行期：

- `showmount -e <server>`（NFSv3/rpcbind）能列导出；**NFSv4-only 的服务器上不适用**（本次的
  `nfs-server-alpine` 就禁用了 v2/v3，只有 2049）。
- 更通用的弱探测是"从控制面 `mount` 一次"——但控制面 Pod 没有特权、也不该有；
  用 `nc -z <server> 2049` 只能证明端口通，**证明不了路径存在**（§6.1 A 就是端口通、路径不存在）。
- 结论：**提交期最多能判断"服务器/端口可达"，路径与权限只能运行期发现**。
  想让用户少等，应该缩短"卡在 ContainerCreating"的观测窗口，而不是加伪 dry-run。

---

## 7. 未确认与风险

| # | 项 | 状态 |
|---|---|---|
| U1 | **CSI（③）没有实跑** | `git clone` / `kubectl apply -k` 到 github.com 被本机网络阻断（`Failed to connect to github.com port 443`）。③的结论全部来自官方文档 + §3.3 的 chown 机制实测，**未经端到端验证**。要闭环需在有外网的环境跑一遍 `csi-driver-nfs` + `fsGroup` + `root_squash` 导出。 |
| U2 | **NFSv3 与 NFSv4 的差异未覆盖** | 本次只测了 NFSv4.2（`vers=4.2`，镜像禁用了 v2/v3）。生产若用 NFSv3，`showmount` 可用、但 mountd/rpcbind 端口要额外放开，且 `no_auth_nlm`/`insecure` 的作用不同。未测。 |
| U3 | **多节点 kind 未测** | 单节点下 `clientaddr` 恒为节点 IP。多节点时 NFS server 的 `PERMITTED` 要放行整个 Pod/节点网段，且每个节点都要有 `mount.nfs`。未测。 |
| U4 | **`kind` 宿主内核依赖是环境特有的** | `nfsd` 模块来自宿主 WSL2 内核；Linux 宿主机上 `modprobe nfsd` 通常直接可用，macOS/Windows 的 Docker Desktop 行为可能不同。不要把"必须 `wsl -d docker-desktop`"写进仓库文档。 |
| U5 | **`ROX` 的语义弱于"真只读"** | 官方文档自己提醒：access mode 只是绑定匹配用，"created as ReadOnlyMany, it is no guarantee that it will be read-only"。真正的只读靠 `volumeMount.readOnly` + 服务端 `ro`，实测两者叠加后写入报 `Read-only file system`。 |
| U6 | **`mountOptions` 改动只对新挂载生效** | 实测能 patch，但没验证"改完之后旧 Pod 重挂会不会用新参数"。需要重建 Pod 才可靠。 |
| U7 | **`testplane-storage`（产物 PVC）本身还是 hostPath** | `deploy/k8s/storage.yaml:44-47`。本次只研究"模型权重"只读挂载，**没有**把产物 PVC 换成 NFS；那会牵出 `subPath` 预创建、属主、`04-process-view.md:253` 的 NFS 抖动风险，是另一张票。 |

---

## 8. 一手来源清单

**K8s 官方文档**

- [Pod Security Standards § Restricted](https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted) —— 卷类型白名单（8 种，不含 `nfs`）
- [Volumes § nfs](https://kubernetes.io/docs/concepts/storage/volumes/#nfs) —— `nfs` 卷字段；"can't specify NFS mount options in a Pod spec"
- [Volumes § Using subPath](https://kubernetes.io/docs/concepts/storage/volumes/#using-subpath) —— `subPath` 与卷类型无关
- [Persistent Volumes § Types of Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#types-of-persistent-volumes) —— `nfs` PV；`/sbin/mount.nfs` 要求
- [Persistent Volumes § Access Modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes) —— NFS 支持 RWO/ROX/RWX；access mode 不等于只读保证
- [Persistent Volumes § Mount Options](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#mount-options) —— 只有 `csi`/`iscsi`/`nfs` 支持
- [Persistent Volumes § Reclaim Policy](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#reclaim-policy) —— 1.37 只有 `nfs`/`hostPath` 支持 Recycle

**K8s 源码（master）**

- [`pkg/volume/nfs/nfs.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/volume/nfs/nfs.go) —— `SetUpAt` 忽略 `mounterArgs`（fsGroup 无效）；`TearDownAt` 注释自述 root_squash；`mountOptions` 走 `util.MountOptionFromSpec`
- [`pkg/volume/volume.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/volume/volume.go) —— `MounterArgs{FsGroup, FSGroupChangePolicy}`；`Attributes` 定义

**csi-driver-nfs**

- [`docs/driver-parameters.md`](https://github.com/kubernetes-csi/csi-driver-nfs/blob/master/docs/driver-parameters.md) —— `fsGroupPolicy: File`、`chown(-1, fsGroup)`、root_squash 导出会"surface as a mount failure"、`uid/gid`/`mountPermissions`/`subDir`

**本仓库**

- `server/internal/executor/render.go:182-213`（安全上下文）、`:513-589`（`buildMounts`）、`:552`（`used` map）
- `server/internal/executor/observe.go:51`、`:253-258`、`:422-430`、`:505-518`（`MOUNT_UNAVAILABLE` 的唯一产出点）
- `server/internal/domain/types.go:206-212`（`MountSpec`）、`server/internal/ports/ports.go:59`（`JobSpec.Mounts`）
- `server/internal/registry/registry.go:81-85`（保留路径）、`:568-576`（解析）、`:642-680`（挂载校验）
- `deploy/charts/testplane/values.yaml:134-140`（`persistence.existingClaim`）、`:223-224`（`storagePort: 2049`）
- `deploy/k8s/storage.yaml:44-47`（产物 PVC 仍是 hostPath）

---

## 9. 引用锚点

| 锚点 | 内容 |
|---|---|
| `R-N:40` | 结论先行（7 条） |
| `R-N:68` | 三种形态对比总表 |
| `R-N:81` | 形态①内联 `nfs` |
| `R-N:110` | 形态② PV `spec.nfs` + PVC |
| `R-N:157` | 形态③ CSI |
| `R-N:180` | PSA 卷白名单是唯一卡点 |
| `R-N:191` | uid 10001 与 `root_squash` 的真实关系 |
| `R-N:230` | `fsGroup` 对 NFS 无效（含源码） |
| `R-N:277` | 导出参数建议 |
| `R-N:307` | kind 节点现状：`mount.nfs` 已有、`nfsd` 没有 |
| `R-N:322` | 宿主内核加载 `nfsd` |
| `R-N:351` | 起 NFS server 的可复现步骤 |
| `R-N:398` | 节点缺 `mount.nfs` 的报错形态 |
| `R-N:428` | PVC 路线：平台零代码改动 |
| `R-N:457` | 内联分支要动的结构 |
| `R-N:471` | 校验不一致（`/tester` vs `/workspace/input`） |
| `R-N:483` | `FailedMount` 事件原文（两种） |
| `R-N:514` | `MOUNT_UNAVAILABLE` 的唯一产出点与 D-60 |
| `R-N:537` | 未确认与风险（U1–U7） |
