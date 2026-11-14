---
name: argus-local-dev
description: 本地 kind 环境操作与排障——集群/端口/kubeconfig 现状、server 代码改后重灌动线、Git Bash 沙箱坑、NetworkPolicy/PSA restricted、port-forward 断连、真 PG 集成测试、Playwright mock 模式。
---

# 本地 kind 环境与排障

## 环境现状（2026-11-14 实测，v2 名词）

- kind 集群名 **argus**（容器 `argus-control-plane`）；kubeconfig **不在默认 config**：
  `KUBECONFIG=D:/Project/TestPalne/.data/kubeconfig-argus.yaml`
- 控制面 `svc/argus`，容器端口 **8080**：`kubectl port-forward svc/argus 8080:8080 -n argus`
- 部署形态是 **kubectl apply 清单，不是 helm release**（chart 是唯一对外部署形态，kind 只调试）
- dev Postgres：docker 容器 `argus-pg`，宿主 5432；
  `postgres://argus:argus@127.0.0.1:5432/argus?sslmode=disable`；psql 走
  `docker exec argus-pg psql -U argus -d argus`

> ⚠️ `docs/runbook/local-kind.md` 的**重灌动线模式仍对，但名词是 v1**（kind-testplane /
> testplaned / testplane-storage）。v2 一律用下面的 argus 名词；跑之前先核对。

## server 代码改后重灌动线（顺序不能换）

```powershell
# 1 构建（Git Bash 里 docker.exe 正常；build-push.ps1 的 -Push 是 switch，传值会污染 tag）
pwsh deploy/docker/build-push.ps1 -Tag dev
# 2 打成本地清单引用的 tag
docker tag ghcr.io/ucm-system/argus-server:dev argus-server:dev-logo
# 3 灌进 kind 节点（kind 节点看不到宿主镜像；这步永远不能省）
pwsh -NoProfile -File deploy/kind/load-image.ps1 -Image argus-server:dev-logo
# 4 拉起新副本
kubectl --kubeconfig .data/kubeconfig-argus.yaml rollout restart deploy/argusd -n argus
# 5 重连 port-forward（restart 必断），再 /healthz /readyz 验活
```

**Git Bash 沙箱坑**：`kind.exe` 直接跑会 pipe permission denied——kind/load-image 一律
`pwsh -NoProfile -File`；`docker.exe`、`kubectl.exe` 正常。

## 排障判据（都是实翻过的车）

- **DNS 通 + connect timeout**：先查 NetworkPolicy（`argus-default-deny` 全拒，端口要显式放行，
  参考 `mock-llm-allow-ingress`），再怀疑**对端进程僵死**——Pod Running + endpoint Ready ≠ 服务活着
  （mock-llm 僵死 44h 才被发现，`rollout restart` 即好；它还没有 liveness probe）。
- **port-forward 抢 8080 bind 失败**：先 `powershell "Get-Process kubectl | Stop-Process"` 清干净再起一个。
- **argus ns PSA restricted**：临时 Pod 必须带完整 securityContext（runAsNonRoot+seccomp+caps drop），
  且 kubectl run 的 --overrides 不可靠；**裸 managed Pod/Job 会被 argusd 对账回收**——
  临时探针直接 `kubectl exec` 进存活的 runner Job pod。
- **产物清单是 DB 行不是活扫描**：task finish 时 collector 写 artifacts 表；事后往存储目录投文件
  不会出现在清单里（演示补录要手 INSERT）。
- **registry 版本钉死**：同 name+version 不同内容启动即拒（见 `argus-develop-runner`）。

## 测试与联调

- **Go 集成测试要真 PG**：`cd server && AG_TEST_DATABASE_URL='postgres://argus:argus@127.0.0.1:5432/argus?sslmode=disable' go test ./internal/api/ -run <Name>`
  （Docker Desktop 得起着，argus-pg 随它自启）。
- **Playwright（97 用例）**：一律让测试框架自起 dev server（注入 `VITE_USE_MOCK=1`），别手动
  `pnpm dev` 后复用——mock 注入缺失会打到真代理。装依赖用 `npx -y pnpm@latest`（本机 pnpm 版本
  与 store 不匹配会 ERR_PNPM_UNEXPECTED_STORE）。
- **改了 server 端口/Service 名**：mock-deploy 按 `mock-llm.argus.svc.cluster.local:18080` 探测，
  Service 名固定 mock-llm。
