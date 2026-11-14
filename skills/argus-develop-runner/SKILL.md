---
name: argus-develop-runner
description: 开发/适配一个 Argus Runner——runners/ 目录结构、runner.yaml 全字段契约、平台注入的环境变量（AG_PARAMS_FILE/AG_OUTPUT_DIR 等）、退出码与产物三选一、exports/consumes、版本钉死规则、镜像构建与注册流程。
---

# 开发适配 Runner

## 目录布局（一个 Runner 一个目录）

```
runners/<name>/
  runner.yaml        # 契约声明（注册表的内容 sha 源之一）
  run.sh             # 入口脚本（entry 指向它）
  build/Dockerfile   # 唯一发布镜像；release.yml 自动发现 runners/*/build/Dockerfile
  run-local.sh       # 可选：本地联调
  tests/ tool/       # 可选
```

**最佳参考样本：`runners/mock-deploy/`**（runner.yaml 与 run.sh 每个字段都有规约条目注释）；
规格权威是 `design/impl/execution.md`（§5 调用契约 / §6 资源 / §7 策略 / §12.4 环境变量）。

## runner.yaml 必须知道的字段

- `role`：**用户只能注册 `test` / `pre_check`**——`deploy` / `clean` 仅内置（注册直接拒绝）。
  clean 阶段的配对靠 deploy 类 runner 声明 `clean_runner`，且必须解析到 role=clean 的 runner。
- `on_failure`：`pre_check` 必须 `abort_task`（否则拒绝注册）；业务 runner 可 `continue`。
- `entry`：exec form 数组，平台直接 exec、**不经 shell、不追加 argv**（D-66）。
- `input_format`：`jsonl`（平台做语法+结构校验，parameters 是 schema）/ `yaml` / `text`（只校语法，parameters 必为 []）。
- `placeholder`：**必须与 input_format 同格式且逐行合法（D-67）**——jsonl = 单行完整 JSON 对象。
  官方示例复制进 UI 就要能过校验，否则就是 bug（踩过）。
- `exports` / `consumes`：组合期闭合校验（§5.12），下游 consumes 必须被上游 exports 或
  service/deploy 导出闭合，否则提交 400。
- `env`：**声明不是绑定**。值来源优先级：Task 级 env > 上游 exports > 本声明 default；
  `required+secret` 的要在 Task 级 `secrets` 给引用名。

## 平台注入的环境变量（执行层强制，§12.4）

| 变量 | 含义 |
|---|---|
| `AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ` | 身份三元组 |
| `AG_PARAMS_FILE` | 输入文件绝对路径（缺省 `/workspace/input/params.jsonl`） |
| `AG_OUTPUT_DIR` | 产物目录（`/workspace/output`，必须写这里才会被收集） |
| `AG_SERVED_URL` / `AG_SERVED_MODEL_NAME` / `AG_MODEL_PATH` / `AG_DEPLOY_REF` | deploy 四件套导出，测试类 runner 直接消费 |
| `TZ`、`HOME=/tmp` | 强制覆写 |

`args` 恒为空数组；挂载只有 input（**只读**）/ output（RW，subPath）/ profile_mount 三种。

## run.sh 契约

- 输入三通道（§5.3）：`--params-file` > `$AG_PARAMS_FILE` > `/workspace/input/params.jsonl`；空输入可以合法（参数全走缺省）。
- 产物写 `$AG_OUTPUT_DIR`：**result.json / result.html / result.csv 三选一**（D-74，全缺 =
  `RESULT_MISSING`）；`logs/` 目录；deploy 类另写 `deploy-state.json`（temp+rename 原子写，
  schema 必须能过 `server/internal/deploystate.Parse`）与 `exports.env`。
- **退出码（§5.8）**：0 成功；1 业务失败（终态 FAILED）；2 参数/输入错误。无运行时重试（D-69）。
- 日志到 stdout/stderr，平台落盘；**不要打印凭据**。

## 版本与注册（最容易翻车的两条）

- **任何内容变化（含 description/placeholder/display_name）都必须 bump `version`**——注册表按
  name+version 钉内容 sha（normalize(runner.yaml)+normalize(run.sh)），同版本不同内容启动即拒
  （`RUNNER_VERSION_PINNED`）。镜像 tag 可以不动（脚本没变就不用重推）。
- **注册 = argusd 启动时扫描 runners/ 目录落库（K-3），无运行时注册**。新 runner = 建目录 +
  重启 argusd（本地 kind 重灌动线见 `argus-local-dev` 技能）。

## 集群侧约束

- PSA restricted：镜像必须非 root（runAsNonRoot + seccomp + caps drop），否则 Pod 起不来。
- NetworkPolicy default-deny：要连的端口必须有放行策略（参考 mock-llm-allow-ingress）。
- `resources.gpu` 用 `nvidia.com/gpu` 计数；纯探测类 runner 给 `100m/64Mi` 就够。
- 本地把镜像灌进 kind 节点：`deploy/kind/load-image.ps1`（ctr-import，Git Bash 里 kind.exe 不可用）。

## 验收动线

1. `runner.yaml` 逐字段对照 mock-deploy 样本；placeholder 用脚本逐行 `json.loads` 校验。
2. 重启 argusd → `GET /api/v1/runners` 看到新版本。
3. `POST /tasks?dry_run=true` 过 → 小参数真提交 → 看该 runner 的 logs 与 result。
