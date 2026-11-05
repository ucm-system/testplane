# 调研证据库（Genet / Volcano）

这个目录放**设计结论的证据原文**。`design/` 里的很多判断（D-44～D-53）不是凭感觉写的，
它们来自对上游 Genet 源码、Jenkins 流水线和 Volcano CRD 的阅读。把笔记放在仓库里，
是为了让读 `main` 的人能**逐条复核**，而不是只能相信文档里的结论。

## 文件

| 文件 | 主题 | 主要支撑 |
|---|---|---|
| [genet-lifecycle.md](genet-lifecycle.md) | Genet 的部署模型：Helm release、拉起/轮询/卸载、状态两根轴、取消与对账、风险清单（R1–R14） | D-44～D-47、D-50、`impl/execution.md §9` |
| [genet-oidc.md](genet-oidc.md) | Genet 的身份体系：企业 OAuth 会话、Open API Key、为什么不能把它当 IdP、安全观察 | D-48、`views/07-api.md §2.1` |
| [volcano-queue.md](volcano-queue.md) | 排队展示的真实来源（Jenkins 下游 → Genet `/api/open/queue` → Volcano CRD）、字段映射与坑 | D-49、`impl/control-plane.md §C-4.8`、`views/06-physical-view.md §2.1` |
| [genet-deploy-plan.md](genet-deploy-plan.md) | 在本机 kind 里部署 Genet 的可行性：内网镜像、cert-manager 硬依赖、固定 chart、最小 values | `deploy/genet-local/README.md` |

## 引用约定

- **`R-G:行号`** = help 于 `genet-lifecycle.md` 的该行（"R-G" 读作 *Research-Genet*）。
  例：`R-G:294` 指 [genet-lifecycle.md](genet-lifecycle.md) 第 294 行。
- **`R-O:行号`** = `genet-oidc.md`；**`R-V:行号`** = `volcano-queue.md`；**`R-D:行号`** = `genet-deploy-plan.md`。
  （新笔记请沿用 "<短名>:行号" 的形式，并在本文件登记。）
- **`<G>/path:行号`** = **上游 Genet 仓库**里的文件与行号。上游是独立仓库，不在本仓库内；
  引用时请写成 `<G>/backend/internal/handlers/helm.go:2906` 这种**全路径**，并在下面这段里说明
  快照信息，读者可以用同一个 commit 拉下来核对。

## 上游快照

调研基于 `ucm-system/genet` 的这份快照：

```
commit  d429cf8656e6
date    2026-11-04T20:30:27+08:00
```

命令：`git clone git@github.com:ucm-system/genet.git && git checkout d429cf8656e6`
（本仓库不 vendoring 上游源码；`<G>` 前缀的所有引用都以该 commit 为准。）

## 维护规则

1. **结论变了就改笔记**，别只改 `design/`——否则证据和结论会互相矛盾（这类矛盾在本项目里真实发生过：D-40 与 `03 §4.1`）。
2. **不要把只在本机能跑的命令输出当证据**：贴关键片段 + 行号，别贴整段日志。
3. **未确认项要写"未确认"**：四份笔记都带"未确认/风险"小节，那是设计里"已知边界"的来源，
   删掉就等于把假设伪装成事实。
