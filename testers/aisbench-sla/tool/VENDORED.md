# 这是什么

本目录是外部仓库 **`aisbench-sla`**（AISBench 前缀缓存 SLA 自动调优 CLI）的**只读副本**（vendored）。
Tester 镜像直接从这里 `COPY`，**不在构建期联网 clone**，也不改上游代码——上游改动靠重新 vendor 一次。

| 项 | 值 |
|---|---|
| 上游仓库（本机） | `D:\Vibe_Workspace\aisbench-sla` |
| vendored commit | `e271cb3bdddb714e526b0f8e5627c036e579d409`（`main`，`docs: README 按使用者顺序重排…`） |
| 上游版本 | `sla_tool.__version__ = "0.7.0"` |
| vendored 时间 | 2026-11-06 |
| License | MIT（见 `LICENSE`）；第三方来源见 `THIRD_PARTY_NOTICES.md` |
| 运行时依赖 | 只有 `requests`（见 `requirements.txt`）；生成数据集另需 `transformers`，本镜像**不装** |

## 为什么 vendor 而不是 clone

`design/ARCHITECTURE.md` §6.2 要求镜像由 CI 产出、可追溯到具体镜像引用；构建期联网 clone 会让
"这次构建到底拉了哪个 commit" 不可复现。vendor 进来后，构建输入就是仓库里的字节。

## 与原仓库的差异

**没有代码改动。** 只做了三件事：

1. 排除 `.git/`、`.rtest/`、`sla_out/`、`__pycache__/`（开发期留下的运行产物，不属于源码）；
   上游的 `.gitignore` 原样保留（现在位于 `tool/.gitignore`）；
2. 新增本文件；
3. 镜像构建时给 `sla_search.py` / `tests/*.py` 加可执行位（`build/Dockerfile`），
   让 `--aisbench_cmd=/tester/tool/tests/stub_aisbench.py` 这种写法能直接跑。

## 怎么升级

```bash
# 1) 在上游仓库切到目标 commit，确认 tests 全绿
py -3.11 tests/test_cli_surface.py
py -3.11 tests/test_offline.py

# 2) 覆盖本目录（保留 VENDORED.md），更新上面的 commit 与版本
# 3) 重跑本 Tester 的静态验证与容器冒烟（见 ../README.md「验证」）
```
