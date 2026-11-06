# 这是什么

本目录是外部仓库 **`aisbench-sla`**（AISBench 前缀缓存 SLA 自动调优 CLI）的副本（vendored）。
Tester 镜像直接从这里 `COPY`，**不在构建期联网 clone**；上游改动靠重新 vendor 一次。
与上游的差异只有下面「本地改动」列出的几处（评审 #62 要求可追溯，见 ARCHITECTURE §6.2）。

| 项 | 值 |
|---|---|
| 上游仓库 | **内部镜像仓库**（当前没有公开地址）；本机开发副本在 `D:\Vibe_Workspace\aisbench-sla` |
| vendored commit | `e271cb3bdddb714e526b0f8e5627c036e579d409`（`main`，`docs: README 按使用者顺序重排…`） |
| 上游版本 | `sla_tool.__version__ = "0.7.0"`（`python -c "import sla_tool; print(sla_tool.__version__)"`） |
| vendored 时间 | 2026-11-06 |
| License | MIT（见 `LICENSE`）；第三方来源见 `THIRD_PARTY_NOTICES.md` |
| 运行时依赖 | 只有 `requests`（见 `requirements.txt`）；生成数据集另需 `transformers`，stub 镜像**不装** |

## 怎么复核这份副本对应哪个上游 commit

公开地址拿不到时，用下面的方式核对（命令在开发副本里执行，不是本仓库）：

```bash
git -C D:/Vibe_Workspace/aisbench-sla rev-parse HEAD
#   → 期望 e271cb3bdddb714e526b0f8e5627c036e579d409
git -C D:/Vibe_Workspace/aisbench-sla cat-file -t e271cb3bdddb714e526b0f8e5627c036e579d409
#   → commit（确认该对象确实存在）
git -C D:/Vibe_Workspace/aisbench-sla diff --stat e271cb3 -- `
#   与下一行是本仓库 tool/ 目录：除「本地改动」列出的文件外应无差异
```

等价的逐文件核对：把 `tool/` 与上游 checkout 的同一路径做 `diff -r`，允许出现的差异只有
「本地改动」里列的文件与 `VENDORED.md` / `THIRD_PARTY_NOTICES.md` 本身。

```bash
# 版本自证（与本仓库 tool/ 里的一致）
python -c "import sys; sys.path.insert(0, 'tool'); import sla_tool; print(sla_tool.__version__)"
#   → 0.7.0
```

## 为什么 vendor 而不是 clone

`design/ARCHITECTURE.md` §6.2 要求镜像由 CI 产出、可追溯到具体镜像引用；构建期联网 clone 会让
"这次构建到底拉了哪个 commit" 不可复现。vendor 进来后，构建输入就是仓库里的字节。

## 本地改动（与上游 `e271cb3` 的差异）

0.4.0 起有代码改动，全部为评审 #62 而做，逐条列在这里：

| 文件 | 改动 | 原因 |
|---|---|---|
| `sla_tool/config.py` | `normalize()` 里 `api_key` 为空时回落到环境变量 `ENDPOINT_API_KEY` | 凭据只走 env，平台/`run.sh` 不再往 argv 里塞 `--api-key`（§12.4、评审 #62 硬问题 1） |
| `sla_tool/aisbench.py` | 模型配置模板不再写明文 `api_key`，改写成 `_API_KEY = os.environ.get("ENDPOINT_API_KEY", "")`；`run_command()` 新增 `env_extra`，只在执行子进程时**内存注入** key | 同一原因：`<work_path>/…/vllm_api_chat_temp.py` 落盘也不含密钥 |
| `sla_tool/search.py` | `_run_phase` 通过 `env_extra` 注入 key；新增 `MEASURED_CONCURRENCY_MIN_RATIO = 0.8`、`PointResult.degraded/degraded_reason` 与 `_mark_degraded()`，并把降级点写进 `warnings` | 「最大并发必须是测出来的」：实测并发 < 目标 80% 的点不能当 PASS 结论（评审 #62 硬问题 2） |
| `sla_tool/report.py` | `sla_points.csv` 新增 `degraded` / `degraded_reason` / `measured_concurrency_ratio` 三列；`sla_summary.json` 新增 `max_ok_concurrency_measured` / `measured_concurrency_min_ratio` / `degraded_points` | 同上；CSV 是给人看证据的地方，必须能一眼看到哪个点没压满 |
| `tests/test_offline.py` | 新增 `DegradedAndCredentialTests`（降级判定、CSV 旁注、summary 只统计达标点、模型配置不含明文 key、env 回落） | 上述两处的回归 |

**语义保持不变**的字段：`max_ok_concurrency` 仍是「配置口径下最大的 PASS 并发」（工具退出码与
搜索策略不变）；`max_ok_concurrency_measured` 才是「实测并发达标」的那个数，`run.sh` 用它
（并独立按同一阈值复核）决定 `result.json` 的 `sla_max_concurrency`。

## 收录范围

1. 排除 `.git/`、`.rtest/`、`sla_out/`、`__pycache__/`（开发期留下的运行产物，不属于源码）；
   上游的 `.gitignore` 原样保留（现在位于 `tool/.gitignore`）；
2. 新增本文件与 `THIRD_PARTY_NOTICES.md`；
3. 镜像构建时给 `sla_search.py` / `tests/*.py` 加可执行位并把 CRLF 擦成 LF（`build/Dockerfile*`），
   让 `--aisbench_cmd=/tester/tool/tests/stub_aisbench.py` 这种写法能直接跑。

## 怎么升级

```bash
# 1) 在上游仓库切到目标 commit，确认 tests 全绿
py -3.11 tests/test_cli_surface.py
py -3.11 tests/test_offline.py

# 2) 覆盖本目录（保留 VENDORED.md），更新上面的 commit 与版本；
#    把「本地改动」按新上游重新套一遍（改动都集中在 config.py / aisbench.py / search.py / report.py）
# 3) 重跑本 Tester 的静态验证与容器冒烟（见 ../README.md「维护者验证清单」）
```
