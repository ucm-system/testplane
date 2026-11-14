# Argus v0.2.0

这一版是一次**大版本**：平台从 TestPlane 更名 **Argus** 并完成 Runner 化改造（#94/#95），随后三波 UX 收口连续落地——服务来源下拉化、结果视图重构、界面去噪音；中间还补上了管理台「挂载点」（D-65）和 D-77（runners 可为 0）。

## 镜像（linux/amd64 + linux/arm64）

镜像名随更名全部从 `testplane-*` 换成 `argus-*`（旧 ghcr 路径不再更新）：

| 镜像 | index digest |
|---|---|
| `ghcr.io/ucm-system/argus-server:0.2.0` | 待发布工作流产出后回填 |
| `ghcr.io/ucm-system/argus-web:0.2.0` | 待发布工作流产出后回填 |
| `ghcr.io/ucm-system/argus-runner-aisbench-sla:0.2.0` | 待发布工作流产出后回填 |
| `ghcr.io/ucm-system/argus-runner-endpoint-probe:0.2.0` | 待发布工作流产出后回填 |
| `ghcr.io/ucm-system/argus-runner-genet-deploy:0.2.0` | 待发布工作流产出后回填 |
| `ghcr.io/ucm-system/argus-runner-genet-clean:0.2.0` | 待发布工作流产出后回填 |
| `ghcr.io/ucm-system/argus-runner-mock-deploy:0.2.0` | 待发布工作流产出后回填 |

## 1. 更名 Argus + Runner 化改造（#94/#95）

- 平台更名 TestPlane → **Argus**：Run → **Task**、Tester → **Runner**、`TP_*` → `AG_*`；UI、文档、API 面全量迁移，新 Logo；
- Runner 定义 v2 化：`runners/` 目录 discover 驱动发布矩阵（加 Runner 不用改 workflow）；
- provider 组合化：`genet-deploy` / `genet-clean` 成为内置 runner，deploy 阶段对接 Genet 的 Helm release 语义，mock-deploy 服务联调；
- Go module 路径仍为 `github.com/ucm-system/testplane`（改名涉及 go.mod 级联，后续单独处理，见仓库 issue）。

## 2. 管理台「挂载点」：运行时增删改统一挂载（D-65，#91/#92）

- 5 个管理端点落库 + 管理台新 tab：NFS/PVC + Pod 内路径，自动注入所有 Runner；
- 提交期同路径冲突 400 `VALIDATION_ERROR`；未配置不阻断；快照冻结语义不变（已提交 Task 不受后续改动影响）。

## 3. 服务来源三选一下拉（D-75，#96–#99）

- 提交页服务来源从手填 YAML 收敛为**三选一**：**Genet（默认）/ Mock / 已有服务（外接，直接填地址）**；
- mock 提供方解耦（可整体关闭）；下拉标签对齐、选项文案去重、页头重复标签清理。

## 4. 结果视图：所有 Runner 一个详情面板 + 重试=带参重提交（D-75/D-76，#96/#101）

- 结果视图收敛：deploy / clean 也进同一个 runner 详情面板（输入 / 导出 / 日志 / 产物一个入口）；
- 整页查看器满高（html > csv > json 渲染优先度）；重试改为**带原参数重提交**，回填可编辑。

## 5. 提交页编辑器三连（#102–#104）

- jsonl placeholder 单行化（官方示例复制即报错的坑）；校验器给「跨行单个 JSON 对象」一键救回；
- jsonl 输入框换 CodeMirror：与 YAML 一致的语法高亮与校验；去掉与「格式化」重复的「压缩」按钮；
- 三种输入框统一行数、内容增长自适应（可拖拽上限 50vh）。

## 6. 界面去噪音 + D-77（#105）

- 删掉所有规格引用灰字（"D-37"、"§12.4" 这类实现细节不再出现在用户界面），文案砍半，主行动提权（重试=primary、返回=ghost）；
- 「详情」按钮统一 ghost 文字（Runners 页 / 提交页三处同款）；
- **D-77：runners 可为 0**——只提交服务来源 + precheck（auto）/ clean，用于验证服务能否启动；服务端（assemble/store/IT）、openapi（删 `minItems: 1`）、前端空态中性说明全链路对齐。

## 7. 其它修复

- `aisbench-sla` 0.4.0 → 0.5.0：同一 0.4.0 版本内容变了导致注册表拒绝启动，改为升版本号（#93）；
- ux-report 内联指标断言放宽到 30s（#100）。

## 升级注意

- **镜像名换了**：`testplane-*` → `argus-*`，旧路径停在 0.1.2 不再更新；
- **chart 版本 0.1.2 → 0.2.0**（0.1.3 只在 main 上短暂存在、未发版）；
- **D-77 契约**：`runners` 允许为空数组（0 个测试 runner 合法）；deploy XOR service 的互斥校验不变——管道永远非空（deploy/precheck/clean 注入）；
- 生产请钉 **digest**：本 PR 先只改 tag，digest 由打 tag 的发布工作流产出后单独回填（同 v0.1.2 → #89 流程）；回填前 digest 仍指 v0.1.2，勿用 release values 部署。
