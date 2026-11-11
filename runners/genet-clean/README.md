# service-clean：服务拆除（内置 clean Runner）

回答一个二值问题：**这个 release 真的拆干净了吗？** 它是部署模式管道的最后一环（finally
语义，§7.6）：成功/失败/取消/超时四条路径都会被排程执行；失败不改 Task 终态，只记
`cleanup_failed` 并进重放清单（§9.6）。

- 角色 `clean`：**仅内置**，用户目录声明 `role: clean` 一律拒绝注册（§5.1）
- 失败扩散 `on_failure: abort_task`（注册校验强制；finally 的执行由平台排程保证）
- 入口 `/runner/run.sh`（由 `runner.yaml` 的 `entry: ["/runner/run.sh"]` 声明，§5.2）
- 契约：[ARCHITECTURE.md](../../design/ARCHITECTURE.md) §5.8、§7.6、§9.1、§5.12；
  [execution.md §9](../../design/impl/execution.md)（§9.2 Genet 事实基线、§9.4 行为契约）

## 目录结构

```text
service-clean/
├── runner.yaml               # 唯一权威：元数据 + entry + input_format: jsonl + consumes + env 声明
├── run.sh                    # 唯一执行入口（平台调它，独立运行也跑它）
├── build/
│   └── Dockerfile            # 构建定义（bash+curl+jq 精简镜像，非 root，上下文 = 本目录）
├── run-local.sh              # 本地便捷脚本（只 run，不 build）
├── tests/
│   ├── run_tests.sh          # 冒烟入口（stub Genet，不需要集群；native/docker 自动选择）
│   └── stub_genet.py         # 假 Genet OpenAPI（形状对齐 tools/fake-genet 与调研接口表）
└── README.md
```

## 调用契约（v2：输入文件 + env，argv 恒空）

| 通道 | 内容 |
|---|---|
| env（必需） | `AG_GENET_BASE_URL`、`GENET_API_KEY`（**必须与部署时同一把**——namespace 由 API Key 绑定，§9.2） |
| env（来源） | `AG_DEPLOY_REF`（deploy 导出的 `namespace/releaseName`，`consumes` 声明） |
| env（可选） | `AG_CLEAN_VERIFY_TIMEOUT`（判干净总窗口秒，缺省 600）、`AG_CLEAN_POLL_INITIAL_INTERVAL=2`、`AG_CLEAN_POLL_MAX_INTERVAL=15`、`AG_DEPLOY_STATE_FILE`（deploy-state 副本路径） |
| env（元信息） | `AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ` |
| 输出目录 | `$AG_OUTPUT_DIR`：`logs/clean.log`、`raw/`（本 Runner 无 result/exports 义务，也不写 deploy-state.json） |

## releaseName 的三个来源（§9.4 第 1 条）

1. **`AG_DEPLOY_REF` 优先**：形态 `namespace/releaseName`，取最后一段；
2. deploy-state.json 副本的 `.release_name`（`AG_DEPLOY_STATE_FILE` 或
   `/workspace/input/deploy-state.json`）；
3. 都缺失 → **确定性兜底** `"arg-" + sha256("${AG_TASK_ID}:${AG_ATTEMPT}")[:12]`（D-44，
   与 deploy 同式），**日志注明 fallback**——deploy 未导出即被取消/失败时（§9.6 重放）靠它。

形态校验 `^[a-z][a-z0-9-]{0,52}$`：不合法 → exit 2 拒绝执行（用错误对象 DELETE 会错杀
无关资源；重放时修正输入即可）。

## 判干净与幂等（§9.4 第 2-3 条，D-50/D-54）

1. `DELETE /api/open/helm/releases/{name}?keepHistory=false&force=true`——**只能**走 Genet
   OpenAPI：直接 `helm uninstall` 会被 admission webhook 拒绝（D-54）。
   - 404 → release 不存在，视为已干净（幂等）；
   - 200 → 删除受理（**200 不算数**，以验证循环确认为准）；
   - 5xx/超时 → 结果未知，交给验证循环判定；
   - 401/403 → 凭据错误，exit 2。
2. **判干净 = 两个条件同时满足、且两次查询本身成功**（重试循环，指数退避 2s→15s，
   窗口 `AG_CLEAN_VERIFY_TIMEOUT` 缺省 600s）：
   - `GET /releases/{name}`（light=true，R12）→ **404**；
   - `GET /releases/{name}/pods` → **items == []**（release 已删后 Genet 仍可查、返回空；
     端点 404 亦按"无 Pod"处理并记日志）。
3. **查询失败 ≠ 为空**（R9）：任何一次查询失败（连接失败/5xx）都**不判成功**，重试到窗口
   用尽——宁可 exit 1 让平台重放，也不误报泄漏已清理。

## 退出码（§5.8）

| 码 | 含义 | 平台侧语义 |
|---|---|---|
| `0` | 已确认干净（含幂等：资源本就不存在） | finally 成功 |
| `1` | 窗口内未确认干净 / Genet 调用失败 | `cleanup_failed` + `CLEANUP_FAILED` 事件；**不改 Task 终态**，进 §9.6 重放清单 |
| `2` | 参数/输入错误（env 缺失、AG_DEPLOY_REF 不合法）、认证失败 401/403 | 平台按 runner 失败处理 |

## 独立运行（§5.10；deploy/clean 属豁免项，但同构可跑）

```bash
docker run --rm \
  -e AG_GENET_BASE_URL=http://genet-backend.genet.svc:8080 \
  -e GENET_API_KEY=... \
  -e AG_DEPLOY_REF=argus-svc/arg-9f3a1c2b7d40 \
  -e AG_OUTPUT_DIR=/workspace/output \
  -v /tmp/out:/workspace/output \
  registry.dev.huawei.com/argus/service-clean:1.0.0
# 或便捷脚本：
AG_GENET_BASE_URL=... GENET_API_KEY=... AG_DEPLOY_REF=argus-svc/arg-... ./run-local.sh
```

## 构建（只发生在 CI）

```bash
cd runners/service-clean
docker build -f build/Dockerfile -t registry.dev.huawei.com/argus/service-clean:1.0.0 .
# 本地联调 tag：docker build -f build/Dockerfile -t argus-service-clean:dev .
```

## 本地冒烟（不需要集群）

```bash
bash runners/service-clean/tests/run_tests.sh
```

覆盖任务验收三条路径 + 幂等补充：已干净（fallback + DELETE 404 → exit 0）/ 需删除
（DELETE 200 → 验证 → exit 0）/ 删除后验证（延迟消失 → 循环重试 → exit 0）/ 判不干净
（窗口用尽 → exit 1）/ 查询失败 ≠ 为空（pods 500 → exit 1，R9）/ AG_DEPLOY_REF 不合法
（exit 2，未发请求）。指向真 genet-local 的联调用 `run-local.sh`。

## 部署注意

- **finally 排程**：本 Runner 由平台在成功/失败/取消/超时四条路径上排程（C-5.7）；泄漏兜底
  走 `POST /api/v1/tasks/{id}/clean` 重放（§9.6），重放时幂等键仍 releaseName。
- **硬规则（D-72）**：Runner Job 不绑 ServiceAccount、不设 ownerReferences（Job 回收的级联
  删除会杀掉活着的被测服务）；凭据走 Task 级 `secret_env`。
- **代理**：脚本 `--noproxy '*'` 直连，避免代理 502 干扰判定。
