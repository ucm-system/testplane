# service-deploy：服务部署（内置 deploy Runner）

对 Genet 说"把这个部署描述装起来，直到服务地址可解析为止"。它是部署模式管道的第一环：
读部署描述 → 幂等拉起 Helm release → 轮询并周期改写 `deploy-state.json` → ready 后导出
服务信息四件套，交给平台收割注入下游（pre_check / test）。

- 角色 `deploy`：**仅内置**，用户目录声明 `role: deploy` 一律拒绝注册（§5.1）
- 失败扩散 `on_failure: abort_task`：部署不成功，后面所有 runner 都没有意义（§7.3）
- 入口 `/runner/run.sh`（由 `runner.yaml` 的 `entry: ["/runner/run.sh"]` 声明，§5.2）
- 契约：[ARCHITECTURE.md](../../design/ARCHITECTURE.md) §5.3、§5.8、§9.1、§5.12、附录 C；
  [execution.md §9](../../design/impl/execution.md)（§9.2 Genet 事实基线、§9.3 行为契约）；
  `server/internal/deploystate/deploystate.go`（deploy-state.json 的解析 schema）

## 目录结构

```text
service-deploy/
├── runner.yaml               # 唯一权威：元数据 + entry + input_format: yaml + exports + env 声明
├── run.sh                    # 唯一执行入口（平台调它，独立运行也跑它）
├── build/
│   └── Dockerfile            # 构建定义（bash+curl+jq 精简镜像，非 root，上下文 = 本目录）
├── run-local.sh              # 本地便捷脚本（只 run，不 build）
├── tests/
│   ├── run_tests.sh          # 冒烟入口（stub Genet，不需要集群；native/docker 自动选择）
│   ├── stub_genet.py         # 假 Genet OpenAPI（形状对齐 tools/fake-genet 与调研接口表）
│   └── validate_deploystate.py  # deploy-state.json 的 deploystate.Parse 等价校验器
└── README.md
```

**这里没有、也不该有**：`deployment.yaml` / `job.yaml`（部署由本 Runner 经 Genet 负责，
特殊运行时需求走 `runner.yaml.k8s` 受限字段，§6.8）；`results/`（容器内固定写
`/workspace/output`，§5.3）。

## 调用契约（v2：输入文件 + env，argv 恒空）

| 通道 | 内容 |
|---|---|
| 输入文件 | 部署描述原文（`input_format: yaml`）。读取优先级：`--params-file` > `$AG_PARAMS_FILE` > `/workspace/input/params.yaml`（§5.3）。**语义原样透传 Genet**（作为 `UcStackInstallRequest.values`），本 Runner 只解析少量展示/路由字段：`package_type`（stack\|pd，缺省 stack，D-53）、`template_path`、`model`、`model_path` |
| env（必需） | `AG_GENET_BASE_URL`（Genet OpenAPI 基地址）、`GENET_API_KEY`（Bearer；Task 级 secrets 注入，§12.4） |
| env（可选） | `AG_DEPLOY_TIMEOUT`（部署总窗口秒，缺省 259200=72h）、`AG_DEPLOY_POLL_FAST_INTERVAL=2` / `AG_DEPLOY_POLL_FAST_WINDOW=30` / `AG_DEPLOY_POLL_SLOW_INTERVAL=15` / `AG_DEPLOY_POLL_NOTFOUND_GRACE=180` |
| env（元信息） | `AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ`（缺省 `local`/`1`/`1`） |
| 输出目录 | `$AG_OUTPUT_DIR`（缺省 `/workspace/output`）：`deploy-state.json`、`exports.env`、`logs/`、`raw/` |

部署描述为**必要业务输入**：三通道皆无、或 values/templatePath 全空（Genet 实测会 400，
§9.2）、或超 100KB（R11）→ 参数预检期退出码 2，不发任何请求。

## 行为要点

**releaseName（D-44，确定性）**：`"arg-" + sha256("${AG_TASK_ID}:${AG_ATTEMPT}")[:12]`，
与平台 `domain.ReleaseName(taskID, attempt)` 同式——同 Task 同 attempt 恒等（幂等键），
跨 attempt 必不同（泄漏重放 §9.6 靠它）。

**幂等拉起（§9.3 第 2 步）**：

1. `GET /api/open/helm/releases/{name}`：200 → 恢复场景，直接进轮询；
2. 404 → `POST /api/open/helm/uc-packages/{stack|pd}/releases`，body =
   `{releaseName, noWait: true, values: <params.yaml 原文>}`（`noWait=true` 恒真：
   `timeoutSec` 是 helm `--wait` deadline，排队超时会误判 failed，§9.2）；
3. POST 202 → 轮询；POST 400/409 → GET 复核当幂等成功（文案含 `already has protected
   metadata` 即同名已存在——Genet 同名再 POST 是 400，不是幂等成功）；复核仍 404 = 请求被真拒
   → `DEPLOY_FAILED`；POST 401/403 → 凭据类错误 → 退出码 2；
4. POST 5xx/超时 → **不断定"未创建"**（Genet 202 后台异步 apply），按受理处理进轮询；
   轮询期 GET 404 在宽限窗（缺省 180s）内 → `phase=accepted`，超出 → `DEPLOY_FAILED`。

**轮询与状态写出**：前 30s 每 2s、之后指数退避至上限 15s；单请求 connect 5s / 总 10s；
每轮把状态**原子写** `$AG_OUTPUT_DIR/deploy-state.json`（mktemp + mv，temp+rename）。
5xx / 连接失败 / 超时一律视为瞬态，**不得断定失败**，窗口内继续轮询。队列水位低频查询
（每 5 轮，best-effort，失败不改旧值）；水位只有计数、没有位次（D-46：`queue.position` 恒 null）。

**就绪判定（两根轴，D-45）**：`genet_status=deployed` **且** `callInfo.endpoint.status=ready`
**且** `endpoint.url` 非空 → 写 `exports.env` 四件套 + `deploy-state` phase=ready，exit 0。
`ready` 只代表地址可解析，可用性由 pre_check runner 判定（R1）。

**失败归类（D-47）**：

| 观测 | failure | exit |
|---|---|---|
| release status ∈ {failed, superseded, uninstalling, uninstalled} 或 endpoint.status=blocked | `{code: DEPLOY_BLOCKED, reason: blocked, message: Genet 原文}` | 1 |
| Genet 调不通 / 5xx 轮询耗尽 / 超窗（`AG_DEPLOY_TIMEOUT`，缺省 259200s） | `{code: DEPLOY_BLOCKED, reason: timeout}`（超窗）；`{code: DEPLOY_FAILED, reason: genet_call_failed}`（创建被拒/认证除外） | 1 |
| 参数/输入错误、凭据缺失或认证失败（401/403） | `{code: DEPLOY_FAILED, reason: invalid_input / genet_auth_failed}` | 2 |

> 注：超窗归类按文档三方口径（execution §9.3 / ARCHITECTURE §7.5 / deploystate fixtures）为
> `DEPLOY_BLOCKED(reason=timeout)` → Task 终态 service_unavailable；DEPLOY_FAILED 仅限 Genet 调用本身失败。
> 归类调整只需改 `deploystate.ErrorKind` 侧映射，无需改产物。

## 与 deploystate.Parse 的 schema 对齐

`server/internal/deploystate/deploystate.go` 的 `Parse`/`validate` 是唯一消费方，本 Runner
写出的每一份 `deploy-state.json` 都满足它（`tests/validate_deploystate.py` 是其等价实现，
冒烟测试逐份校验）：

| deploy-state 字段 | deploystate 侧约束 | 本 Runner 行为 |
|---|---|---|
| `phase` | 必填，∈ {queued, accepted, deploying, ready, blocked}（五值，没有 deploy failed 子态，D-47） | 全程只写这五个值；Genet 终态失败折叠为 blocked |
| `release_name` | 非空时须匹配 `^[a-z][a-z0-9-]{0,52}$` | `arg-` + 12 位小写 hex，恒匹配 |
| `genet_status` / `endpoint_status` | 原值保留（排障要能区分"Genet 说 deployed、平台说 deploying"，§9.5） | 直接取 `status` / `callInfo.endpoint.status`，不归一化 |
| `waited_seconds` | ≥ 0 | `now - start`，非负 |
| `pods[]` | `name` 非空；`restarts` ≥ 0 | 取 Genet `pods[]`（name/nodeName/ready/restarts），restarts 负值钳 0；`role` 是按名字约定的展示性提示 |
| `queue` | 计数 ≥ 0；`position` 恒 null（D-46） | `/api/open/queue` 的 `queue.pending/inqueue/running`，负值钳 0，`position: null` |
| `exports` | 键 `^[A-Za-z_][A-Za-z0-9_]*$`，值不含换行 | ready 时的四件套副本，写前 `tr -d '\r\n'` |
| `failure` | code ∈ {DEPLOY_FAILED, DEPLOY_BLOCKED} | 见上表 |
| `values_digest` / 未知字段 | Parse 放行未知字段（前向兼容，§9.5） | 记 spec sha256 前 16 位（R11） |
| `namespace` / `label_selector` / `reason` | 无词法约束 | namespace 取 Genet 返回（API Key 绑定值，不自行推导，R7）；label_selector=`helm-release-name=<release>`（Pod 日志发现依据，E-7.2）；reason 带启发式判据说明（R3） |

## exports（§5.12）

ready 时写 `$AG_OUTPUT_DIR/exports.env`（平台仅在退出码 0 时收割，词法解析失败整体拒收）：

```text
AG_SERVED_URL=http://arg-<sha12>-unified-cache-stack.argus-svc.svc.cluster.local:18080
AG_SERVED_MODEL_NAME=Qwen3-32B      # callInfo.model → 描述里的 model → releaseName（记日志）
AG_MODEL_PATH=/models/Qwen3-32B     # 描述里的 model_path（原样；可达性由挂载决定）
AG_DEPLOY_REF=argus-svc/arg-<sha12> # namespace/releaseName；service-clean 消费
```

凭据**不会**出现在 exports 与日志（GENET_API_KEY 只进 Authorization 头，§12.4/K-5）。

## 独立运行（§5.10；deploy/clean 属豁免项，但同构可跑）

```bash
# 镜像由 CI 产出，只 pull + run（§6.2）
docker run --rm \
  -e AG_GENET_BASE_URL=http://genet-backend.genet.svc:8080 \
  -e GENET_API_KEY=... \
  -e AG_OUTPUT_DIR=/workspace/output \
  -v /tmp/out:/workspace/output \
  -v /tmp/params.yaml:/workspace/input/params.yaml:ro \
  registry.dev.huawei.com/argus/service-deploy:1.0.0
# 或便捷脚本（.local/params.yaml 自动挂载）：
AG_GENET_BASE_URL=... GENET_API_KEY=... ./run-local.sh
```

## 构建（只发生在 CI）

```bash
cd runners/service-deploy
docker build -f build/Dockerfile -t registry.dev.huawei.com/argus/service-deploy:1.0.0 .
# 本地联调 tag：docker build -f build/Dockerfile -t argus-service-deploy:dev .
# 内网/受限网络可加 --build-arg APK_MIRROR=... 换 apk 源（默认官方 CDN）
```

## 本地冒烟（不需要集群）

```bash
bash runners/service-deploy/tests/run_tests.sh
```

自动选择执行方式：宿主机有 bash+curl+jq（≥1.6）时 native 直跑 `run.sh`；否则构建
`argus-service-deploy:dev`（build/Dockerfile）并在容器内跑，stub 经
`host.docker.internal` 可达。覆盖：ready / 幂等恢复（GET 200 不重发 POST）/ POST 400
冲突复核 / helm failed → DEPLOY_BLOCKED / endpoint blocked → DEPLOY_BLOCKED / 连接失败
超窗 → DEPLOY_FAILED / 缺描述与空描述 → exit 2。指向真 genet-local 的联调用 `run-local.sh`。

## 部署注意

- **网络**：Runner Job 必须能达 `AG_GENET_BASE_URL`；脚本 `--noproxy '*'` 直连（代理 502 会被误判成 Genet 故障）。
- **凭据**：`GENET_API_KEY` 走 Task 级 `secret_env`（D-72），Runner 不绑 ServiceAccount、不直连 K8s（§12.2）。
- **窗口**：平台 runner 超时上限（86400s）小于 72h 时，以平台 `runners[].timeout_seconds` 先到为准（§6.6）；
  窗口被平台终止时平台按最近快照归类（C-5.3）。
