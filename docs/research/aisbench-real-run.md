# 真实 ais_bench 的兼容性与 tokenizer 注入

> wayfinder research ticket **#55** 的产物。研究对象：把外部仓库 `D:\Vibe_Workspace\aisbench-sla`
> 封装成的 Tester [`testers/aisbench-sla/`](../../testers/aisbench-sla/)，从 **stub 模式** 走向
> **真实 ais_bench 模式** 所需的镜像事实、服务契约、tokenizer 注入方式与日志口径。
>
> 引用约定见 [README.md](README.md)：`R-A:行号` = 本文件行号；
> `镜像:<容器内路径>:行号` = 官方镜像里的 ais_bench 源码（快照见下）；
> `runN/...` = 真跑产物，存放在 `.data/aisbench-research/runN/`（**不入库**，复现命令见 §7）。

## 0. 方法与证据分级

| 标记 | 含义 | 本文件中的例子 |
|---|---|---|
| **实测** | 真跑出来的命令输出 / 日志，可逐字复核 | 镜像内 `id`、`urllib.parse.urljoin` 打印、真跑日志字节数 |
| **读源码** | 读一手源码 + 行号，未运行 | `buildMounts()` 只认 PVC、`_get_service_probe` 的 `/v1/models` 分支 |
| **推断** | 由上述两条推出、但没有直接跑过 | 「平台上 tokenizer 走挂载可行」在 kind 里的最终形态 |

**镜像快照**（两个都在本机，本文除非注明均指前者）：

```
ghcr.io/aisbench/aisbench_benchmark:v3.1-20260330-master_x86_64_py_310   id 20baa9b1044a  6.39GB
ghcr.io/aisbench/aisbench_benchmark:v3.1-20260630-master-ubuntu24.04-py312-x86_64  id 2b7739e4d5c5  5.8GB
```

ais_bench 源码根在镜像的 `/benchmark`（就是 pip 装的那个包本身）。

---

## 1. 官方镜像内部事实（**实测**）

命令形如
`docker run --rm --entrypoint sh <img> -c '…'`，输出摘录如下。

### 1.1 `/benchmark` 就是 ais_bench 源码根

```
$ pwd            # 镜像默认 WorkingDir
/benchmark
$ ls /benchmark
LICENSE  MANIFEST.in  README.md  README_en.md  Third_Party_Open_Source_Software_Notice
ais_bench  docs  img  plugin_examples  requirements  requirements.txt  setup.py
smoke_tests  tests  tools
$ command -v ais_bench
/usr/local/bin/ais_bench
```

`docker image inspect` 给出 `WorkingDir=/benchmark`、`Cmd=[python3]`，**没有 `User`**。

### 1.2 运行身份：官方镜像是 **root**，不是 uid 10001 —— 修正 ticket 的前提

```
$ id
uid=0(root) gid=0(root) groups=0(root)
$ echo $HOME
/root
```

ticket 写的是「非 root（uid 10001）」，**只对 `testers/aisbench-sla/build/Dockerfile.aisbench`
构建出来的派生镜像成立**（那里有 `USER ${TESTER_UID}:${TESTER_GID}`），官方镜像本身是 root。
因此「uid 10001 能不能写 `/benchmark`」这个问题在官方镜像里是假问题，真正的问题在派生镜像里。

用 `setpriv` 降权到 10001 复测（这才是平台 Job 的实际身份，§4.1 也是 10001）：

```
$ ls -ld /benchmark /tmp
drwxr-xr-x 12 root root 4096 Apr 14  2026 /benchmark
drwxrwxrwt  1 root root 4096 Apr 14  2026 /tmp

$ setpriv --reuid=10001 --regid=10001 --clear-groups sh -c "touch /benchmark/_wtest"
touch: cannot touch '/benchmark/_wtest': Permission denied        → BENCHMARK_WRITE_FAIL
$ setpriv --reuid=10001 --regid=10001 --clear-groups sh -c "mkdir -p /tmp/_wt"
TMP_WRITE_OK
```

**结论：uid 10001 写不了 `/benchmark`（0755 root:root），能写 `/tmp`（1777）。**
这和 `run.sh` 把 `work_path` 默认放 `/tmp/aisbench-ws`、并用
[`run.sh:555`](../../testers/aisbench-sla/run.sh) 导出 `AIS_BENCH_DATASETS_CACHE` +
[`run.sh:556-558`](../../testers/aisbench-sla/run.sh) 追加 `--config-dir` 的做法是自洽的。

### 1.3 Python 与依赖

```
$ python3 -V
Python 3.10.18                                  # ENV PYTHON_VERSION=3.10.18
$ python3 -c "import requests,transformers,datasets;…"
requests 2.33.1   transformers 4.57.6   datasets 3.6.0
$ python3 -c "import tokenizers;print(tokenizers.__version__)"
0.22.2
$ python3 -c "from transformers import AutoTokenizer;print('AutoTokenizer ok')"
AutoTokenizer ok
```

**0630 那版漂移很大**（同一命令，`…20260630-…py312…`）：

```
Python 3.12.3   transformers 5.12.1   requests 2.34.2
```

transformers 4.57.6 → 5.12.1 是主版本跃迁，`AutoTokenizer` 行为与缓存布局都可能变。
**建议钉 0330 版**（`Dockerfile.aisbench` 的 `FROM` 已经钉的这一版）。

### 1.4 工具链

```
$ for b in bash jq curl wget git; do command -v $b || echo "MISSING $b"; done
/bin/bash
MISSING jq
/usr/bin/curl
/usr/bin/wget
/usr/bin/git
```

**没有 `jq`** —— 这正是 `run.sh` 用内嵌 python 写 `result.json` 而不是 `jq` 的原因
（[`build/Dockerfile`:2-5](../../testers/aisbench-sla/build/Dockerfile) 头部注释的同一结论）。
`curl` 在，所以端点探活可以直接用 curl；但 `endpoint-probe` 镜像里装的是 `jq`+`curl` 全家桶，
两者不要混用。

---

## 2. 真 `ais_bench` 对被测算服务的要求

### 2.1 endpoint 拼接：只打 `/v1/chat/completions`（**读源码**）

```python
# 镜像:/benchmark/ais_bench/benchmark/models/api_models/vllm_custom_api_chat.py:95-99
def _get_url(self) -> str:
    endpoint = "v1/chat/completions"
    url = urllib.parse.urljoin(self.base_url, endpoint)
```

```python
# 镜像:/benchmark/ais_bench/benchmark/models/api_models/base_api.py:109-128
def _get_base_url(self) -> str:
    protocol = "https" if self.enable_ssl else "http"
    if self.url:
        self.logger.info(f"Using custom URL: [{self.url}], [host_ip: …] … will be ignored")
        if self.url.startswith("http://") or self.url.startswith("https://"):
            return self.url                      # ← url 字段原样使用，末尾有没有 "/" 很关键
        return f"{protocol}://{self.url}"
    …
    base_url = f"{protocol}://{host}:{self.host_port}/"   # ← host_ip/host_port 分支永远补 "/"
    return base_url
```

`urljoin` 对末尾斜杠敏感（**实测**，镜像内 python3）：

```
'http://h:8000/'    -> http://h:8000/v1/chat/completions
'http://h:8000'     -> http://h:8000/v1/chat/completions
'http://h:8000/v1'  -> http://h:8000/v1/chat/completions
'http://h:8000/v1/' -> http://h:8000/v1/v1/chat/completions     ← 双 /v1
'http://h:8000/openai/' -> http://h:8000/openai/v1/chat/completions   ← 前缀路径可用
```

对照 [`testers/aisbench-sla/run.sh:527-531`](../../testers/aisbench-sla/run.sh) 去掉末尾 `/`
和末尾 `/v1` 段——**做法正确**，否则就是上表第 4 行。

`/v1/completions` 只有非 chat 的 `VLLMCustomAPI` 会打
（`镜像:…/vllm_custom_api.py:75`），本 Tester 的 `API_TYPES` 只有 chat 两档
（[`sla_tool/aisbench.py:59-62`](../../testers/aisbench-sla/tool/sla_tool/aisbench.py)），
**所以 `/v1/completions` 用不到**。

### 2.2 是否需要 `/v1/models`：只在 `model` 为空时才要（**实测 + 读源码**）

```python
# 镜像:…/vllm_custom_api_chat.py:90-91
self.model = model if model else self._get_service_model_path()
```

```python
# 镜像:…/base_api.py:130-147
url = osp.join(self.base_url, "v1/models")
response = requests.get(url, headers=headers, timeout=5)
if response.status_code == 200:
    data = response.json()
    model_id = data['data'][0]['id']        # ← 必须是 {"data":[{"id":…}]}
```

实测（在 0.2.0 验证镜像里构造模型对象）：

```
EXPLICIT_MODEL -> 'mock-llm' | url= http://host.docker.internal:18099/v1/chat/completions
EMPTY_MODEL    -> auto-detected: 'mock-llm'
```

**`model` 给了就不访问 `/v1/models`；没给才自动探测。** 本 Tester 的模板永远写
`model="__MODEL_NAME__"`（[`aisbench.py:42`](../../testers/aisbench-sla/tool/sla_tool/aisbench.py)），
`--model_name` 是公共参数（Run 必须给），所以**平台路径上不依赖 `/v1/models`**。

⚠️ 但工具自己在压测前会探活模型：真跑日志里有
`目标服务可达：http://…（模型：mock-llm）`（`run1/sla_out/run.log`），那是
`sla_tool` 自己打的 `/v1/models`，**不是 ais_bench 打的**。这条探活是硬依赖：
`/v1/models` 不通该点直接失败。

### 2.3 是否读 `/metrics`：**完全不读**（**实测 grep**）

```
$ grep -rn "cache_hit\|prefix_cache\|gpu_cache_usage\|num_cached_tokens" --include=*.py ais_bench/benchmark/
（无匹配）
$ grep -rn "metrics" --include=*.py ais_bench/benchmark/ | grep -iv "metric_calculator\|metrics\.\|_metrics"
（只有 calculator 内部的统计口径，没有任何 HTTP 抓取）
```

ais_bench 的 `metrics` 全是**它自己从响应里算出来的**（TTFT/TPOT/吞吐），跟 Prometheus 无关。

**前缀缓存命中率不是 ais_bench 给的**，是 `sla_tool` 自己在每点前后抓 `/metrics` 差分算的
（`tester.yaml` 的 `description` 也这么写：「命中率按目标服务 /metrics 快照差分给出（只作证据，
不参与判定）」）。所以「命中率」这一列对**被测算服务**是硬要求（要有 vLLM 风格的
`vllm:prefix_cache_queries_total` 等指标），对 ais_bench 不是。

### 2.4 响应的硬形状要求（**读源码**，失败是 `KeyError` 而不是好错误）

```python
# 镜像:…/vllm_custom_api_chat.py:140-154
async def parse_stream_response(self, json_content, output):
    for item in json_content.get("choices", []):
        if item["delta"].get("content"):            # ← delta 必须存在
            output.content += item["delta"]["content"]
async def parse_text_response(self, json_content, output):
    for item in json_content.get("choices", []):
        if content:=item["message"].get("content"): # ← message 必须存在（非流式）
async def _parse_usage(self, json_content: dict, output: Output):
    if json_content.get("usage"):
        output.input_tokens  = json_content["usage"].get("prompt_tokens", 0)     # :136-138
        output.output_tokens = json_content["usage"].get("completion_tokens", 0)
```

```python
# 镜像:…/vllm_custom_api_chat.py:130-131
if self.stream:
    request_body["stream_options"] = {"include_usage": True}
```

即：**流式必须回最后一个 `usage` 帧**，否则 token 计数全是 0、吞吐全是 0。
非流式则必须给 `usage` 块。

### 2.5 `request_rate` / `num_warmups` 字段名与版本差异（**实测 + 读源码**）

| 事实 | 证据 |
|---|---|
| `request_rate` 写在**模型配置 dict** 里，不是 CLI | 生成配置 `models=[dict(… request_rate=0.0 …)]`（`run1/sla_out/points/c1/2026…/configs/*.py:85`） |
| `request_rate=0` 会告警但按「全并发同时发」处理 | `run1/…/full.log`: `The request rate is below 0.1…` → `Request rate (0.0) … < 0.001, sending all requests simultaneously` |
| `--num-warmups` 是**CLI 参数**，两版都有 | `镜像:ais_bench/benchmark/cli/argument_parser.py:119`（0330）；0630 版 `--help` 同样列出 |
| 不传时默认 warmup=1 | `镜像:ais_bench/benchmark/tasks/openicl_api_infer.py:103`: `self.warmup_size = self.cli_args.get("num_warmups", 1)` |
| 工具传 `--num-warmups 0` 生效 | `run1/…/full.log`: `Warmup size is 0, skip...`（命令见 [`aisbench.py:147-148`](../../testers/aisbench-sla/tool/sla_tool/aisbench.py)） |
| 并发走 `batch_size`，不是 CLI | 生成配置 `batch_size=1`（同上 `:76`），由 [`aisbench.py:50`](../../testers/aisbench-sla/tool/sla_tool/aisbench.py) 写入 |
| 模型类被弃用但仍可用 | `VLLMCustomAPIChatStream` 在 `镜像:…/vllm_custom_api_chat.py:176-181`，仅 `logger.warning`；真跑日志里确实出现 `VLLMCustomAPIChatStream is deprecated, please use VLLMCustomAPIChat with stream=True instead.` |

`host_ip` 校验雷区也复核了（[`sla_tool/target.py:5-12`](../../testers/aisbench-sla/tool/sla_tool/target.py)
的说法准确）：

```python
# 镜像:/benchmark/aisbench/benchmark/utils/config/build.py:72-79
if key == "host_ip":
    if value == "localhost":
        continue
    try:
        ipaddress.ip_address(value)
    except ValueError:
        errors[key] = "host_ip must be a valid IPv4 or IPv6 address"
```

### 2.6 tokenizer 为什么是**硬要求**（**实测 + 读源码**）

ais_bench 的 API 模型**不加载 tokenizer**（`BaseAPIModel` 不碰它），但 `default_perf`
summarizer **一定要**：

```python
# 镜像:/benchmark/aisbench/benchmark/summarizers/default_perf.py:259
load_tokenizer(tokenizer_path=model_cfg.get("path"))
```

```python
# 镜像:/benchmark/aisbench/benchmark/utils/file/load_tokenizer.py:29-33 / :40
if not os.path.exists(tokenizer_path):
    raise FileOperationError(UTILS_CODES.TOKENIZER_PATH_NOT_FOUND, …)   # UTILS-FILE-002
…
raise FileOperationError(…)                                             # UTILS-FILE-003
```

实测两种失败形态：

- 路径**不存在** → 工具自己先拦：`--model-path 目录不存在`（
  [`sla_tool/config.py:243-244`](../../testers/aisbench-sla/tool/sla_tool/config.py)），根本走不到 ais_bench。
- 路径**存在但不是 tokenizer** → ais_bench 报 `[UTILS-FILE-003]tokenizer load failed`：

```
FAIL c=1  … 进程退出码 1 …
File "/benchmark/ais_bench/benchmark/utils/file/load_tokenizer.py", line 40, in load_tokenizer
  raise FileOperationError(
ais_bench.benchmark.utils.logging.exceptions.FileOperationError:
  [UTILS-FILE-003]tokenizer load failed. Failed to load tokenizer from /tester/fixture
```

（`run5-badtok/sla_out/…`；同时注意它把该点判 FAIL、最终 `SLA_NO_FEASIBLE_CONCURRENCY` rc=3，
**不是** TESTER_ERROR——因为失败被吞进了单点结果。）

### 2.7 `mock_vllm.py` 能不能满足？逐条对照

| # | ais_bench / 工具的要求 | mock 现状 | 判定 |
|---|---|---|---|
| 1 | `POST /v1/chat/completions` | `mock_vllm.py:238-239` 路由 → `_handle_chat()` | ✅ |
| 2 | 流式帧必须带 `choices[].delta` | `mock_vllm.py:313-321` 每帧都有 `delta` | ✅ |
| 3 | 流式要回 `usage`（`stream_options.include_usage`） | `mock_vllm.py:286-287` 读开关，`:329-336` 发 usage 帧 | ✅ |
| 4 | 非流式要 `choices[].message` + `usage` | `mock_vllm.py:342-351` | ✅ |
| 5 | `model` 空时要 `GET /v1/models` 且形如 `{"data":[{"id":…}]}` | `mock_vllm.py:230-235` | ✅ 实测自动探测成功 |
| 6 | 工具探活打 `/v1/models` | 同上 | ✅ |
| 7 | 命中率要 `/metrics`（vLLM 指标名） | `mock_vllm.py:147-175` 渲染 `vllm:prefix_cache_{queries,hits}_total` 等 | ✅ |
| 8 | `output_len >= 2` 才有 TPOT | `mock_vllm.py:47` 默认 4，且按 `max_tokens` 逐 token 发帧 | ✅ |
| 9 | 高并发下连接复用 | `protocol_version = "HTTP/1.0"`（`:180`）→ **每条请求关连接** | ⚠️ 能跑，但与真 vLLM 的 keep-alive 不同，高并发下 TTFT 会被连接建立污染 |
| 10 | token 计数口径 | `_estimate_prompt_tokens()` 按 4 字符/token 估（`:51-63`），不是真 tokenizer | ⚠️ 输入 token 数与 ais_bench 侧 tokenizer 计数不一致，吞吐量级会偏 |
| 11 | 错误/超时注入、`retry=2` 语义 | 无 | ⚠️ 测不出重试路径 |
| 12 | `/v1/completions` | 无 | ➖ 本 Tester 不用 |
| 13 | `pressure` / ramp 模式 | 无 | ➖ 本 Tester 不用 |

**结论：mock 满足真实 ais_bench 的全部硬要求**（1–8 全部实测通过：真跑 4 次，每次 8 请求，
全部成功）。三个 ⚠️ 是**保真度**问题而不是**兼容性**问题：拿它验证管道可以，拿它做性能结论不行
（尤其 #9/#10）。

---

## 3. tokenizer 注入的三种方式

### 3.0 先厘清 `mounts` 在平台里的**实际**语义（**读源码**）

这是本文最容易踩坑的一节。**`tester.yaml.mounts` 会被真的渲染成 Job 挂载**，但字段和
`design/` 写的不一样。

**实现侧（代码是真相）**：

```go
// server/internal/domain/types.go:204-210
type MountSpec struct {
    Name      string `json:"name" yaml:"name"`
    MountPath string `json:"mount_path" yaml:"mount_path"`
    ReadOnly  bool   `json:"read_only,omitempty" yaml:"read_only"`
    Source    string `json:"source,omitempty" yaml:"source"`   // ← PVC claim 名
}
```

```go
// server/internal/executor/render.go:552-587（buildMounts 的 tester-mounts 段）
for i, m := range spec.Mounts {
    if m.MountPath == ""            { … "mount_path is empty" }
    if !path.IsAbs(m.MountPath)     { … "mount_path must be absolute" }
    if _, dup := used[m.MountPath]; dup { … "conflicts with a platform mount" }
    claim := m.Source
    if claim == "" { claim = spec.StoragePVC }        // :573-576 空 → 产物 PVC
    mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
    volumes = append(volumes, corev1.Volume{Name: name,
        VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &…{ClaimName: claim}}})   // ← 只有 PVC
}
```

链路：`tester.yaml.mounts` → [`registry.go:537-545`](../../server/internal/registry/registry.go) →
`Task.Mounts` → [`wire.go:170`](../../server/cmd/testplaned/wire.go) →
[`controller.go:690`](../../server/internal/orchestrator/controller.go) `Mounts: task.Mounts` →
`buildMounts()`。

**关键事实（全部读源码）**：

1. **只支持 PVC，没有 hostPath**。`buildMounts` 只会生成 `PersistentVolumeClaimVolumeSource`；
   `design/ARCHITECTURE.md:1754` 也明文「**禁止** … `hostPath` …」。要在 kind 里用宿主机目录，
   必须先造一个 `hostPath` 的 **PV**，再绑一个 **PVC**，平台只认那个 PVC 名。
2. **没有 `required`，也不会产生 `MOUNT_UNAVAILABLE`**。而规格
   [`ARCHITECTURE.md:1228`](../../design/ARCHITECTURE.md) /
   [`:1717`](../../design/ARCHITECTURE.md) /
   [`:1753`](../../design/ARCHITECTURE.md) 写的是 `mounts: [{name, path, required}]`
   「由 Execution Profile 提供对应供给；供给缺失时 Task 失败并报 `MOUNT_UNAVAILABLE`」。
   → **规格与实现不一致**：实现里 `mounts[]` 是「我要哪个 PVC 挂到哪」，不是「我需要平台供给什么」。
   Execution Profile 的 `mounts`（`[{name,pvc,sub_path,kind}]`，[`profiles.go:14`](../../server/internal/api/profiles.go)）
   只是**能力清单**（前端渲染用），**不参与 Job 渲染**——`jobSpec()` 只取 `task.Mounts`。
3. **保留路径**（[`registry.go:81-85`](../../server/internal/registry/registry.go)）：
   `/workspace/output`、`/tmp`、`/tester` 不能声明。`/models` 可以。
4. **`mount_path` 必须绝对且不能互相嵌套**（`registry.go:619-636`）。
5. 渲染出来的 Pod 是 `readOnlyRootFilesystem: true` + `runAsUser/Group 10001` + `fsGroup 10001`
   （[`render.go:182-186`](../../server/internal/executor/render.go)），`/tmp` 是 256Mi 的 emptyDir
   （[`render.go:69-70`](../../server/internal/executor/render.go)、`:516`）。

> **可操作的结论**：`tester.yaml` 里写
> `mounts: [{name: models, mount_path: /models, read_only: true, source: <pvc名>}]`
> 就能让 Job 在 `/models` 挂上那个 PVC。**但 `source` 必须指向一个已存在的 PVC**，
> 平台不会替你去 profile 里找。

### 3.1 三种方式对比

| | ① 烤进镜像 | ② kind hostPath PV+PVC + `tester.yaml.mounts` | ③ 放进产物 PVC + 绝对路径 |
|---|---|---|---|
| 做法 | `COPY …/Qwen3-0.6B /models/Qwen3-0.6B` | 节点上放 tokenizer → PV(hostPath) → PVC → `mounts: [{mount_path:/models, source:<pvc>}]` → `--model_path=/models/Qwen3-0.6B` | tokenizer 落 `testplane-storage` PVC，Job 里从 `/workspace/output/...` 读 |
| 体积代价 | **+15.1MB**（`D:\Models\Qwen3-0.6B` 实为 6 个文件 15,087,430 B） | 0（不在镜像里） | 0 |
| 平台侧改动 | 无 | **要改 `tester.yaml`（加 mounts）+ 要在集群里造 PV/PVC** | 无 |
| 能否切不同模型 | ❌ 换 tokenizer 就要重打镜像 | ✅ 换 PVC/内容即可 | ⚠️ 见下 |
| **实测结论** | ✅ 真跑成功（0.2.0 镜像，`--model_path=/models/Qwen3-0.6B`） | ✅ 等价形态实测成功（只读 bind mount 到 `/opt/models`，`--model_path=/opt/models/Qwen3-0.6B`，PASS） | ❌ 路径上不可共享（见下） |
| 主要代价 | 镜像膨胀 + 「生产镜像该不该带 tokenizer」的语义污染 | 需要集群管理员造 PV/PVC；hostPath 把节点目录绑死，多节点不成立 | **每次 Attempt 一个 subPath，tokenizer 得跟着 Attempt 复制** |

**③ 为什么不成立（读源码）**：产物挂载是
`{Name: "output", MountPath: "/workspace/output", SubPath: subPath}`，`subPath` =
`<run>/tasks/<task>/attempts/<n>`（[`render.go:309-317`](../../server/internal/executor/render.go)、
`:514-517`）。也就是说 `/workspace/output` **只是本次 Attempt 自己的目录**，
下一个 Attempt 看不到上一份 tokenizer；要复用就得每次复制 15MB 进去，还挤占产物空间
（`run.sh` 的 50MB 截断阈值就不是给模型文件用的）。

### 3.2 推荐

**分两件事，别混成一件：**

1. **本地/kind 验证 → 用 ①（烤进验证镜像）**。理由：零集群依赖、可复现、15MB 可以接受，
   而且**已经建好了**：[`build/Dockerfile.aisbench`](../../testers/aisbench-sla/build/Dockerfile.aisbench)
   已经 `COPY testers/aisbench-sla/build/models/Qwen3-0.6B /models/Qwen3-0.6B`，
   `build/models/` 在 [`build/.gitignore`](../../testers/aisbench-sla/build/.gitignore) 里不入库。
   镜像 `ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.2.0`（6.43GB）已在本机，§5/§6 的真跑全用它。
   **注意**：这要求 `build/models/Qwen3-0.6B` 在构建时存在，否则 `COPY` 直接失败（故意的）。
2. **生产 → 用 ②（Profile/集群提供 PVC）**，并且**要把 `tester.yaml` 的 `mounts` 从 `[]` 改成
   声明 `/models`**，同时**接受规格要改**：现在的实现没有 `required` / `MOUNT_UNAVAILABLE`，
   与 `ARCHITECTURE §10.4` 的描述对不上——这条应该回到 ticket（要么改实现补 `required`，
   要么改规格承认「mounts = PVC 绑定」）。
   **不要**用 ③：per-Attempt subPath 决定了它不能当模型仓库。

⚠️ 一句话提醒：**现在 `tester.yaml` 里 `mounts: []` 是有注释解释的**
（「声明成固定 mount_path 反而把某个 Profile 的目录布局写死」，见
[`tester.yaml`](../../testers/aisbench-sla/tester.yaml) 的注释）。基于 §3.0 的实现事实，
那个理由**不成立**：`mounts` 的 `mount_path` 是**容器内**路径（不是宿主布局），
写死 `/models` 并不会把宿主目录布局写死。建议改。

---

## 4. `precheck: auto` 缺镜像的问题

### 4.1 事实

- [`testers/endpoint-probe/tester.yaml:20`](../../testers/endpoint-probe/tester.yaml)：
  `image: registry.dev.huawei.com/testplane/endpoint-probe:1.0.0`
- 这个引用**曾经**本地 kind 里没有 → 上轮只能 `precheck: off`。
- **现在已经有**（`crictl images`，node `testplane-control-plane`）：

```
registry.dev.huawei.com/testplane/endpoint-probe   1.0.0   1390b87699f09   7.61MB
docker.io/testplane/endpoint-probe                 1.0.0   1390b87699f09   7.61MB
ghcr.io/ucm-system/testplane-tester-aisbench-sla   0.1.0   a2b84af530f3e   51.6MB
ghcr.io/ucm-system/testplane-tester-aisbench-sla   0.1.1   a4acc51825264   51.6MB
```

### 4.2 「构建同名 tag 再导入」是否可行：**可行，实测通过**

关键前提（**读源码**）：平台对 tag 形态的镜像用 `IfNotPresent`，不会去拉：

```go
// server/internal/executor/render.go:339-352
func imagePullPolicy(image string) corev1.PullPolicy {
    if strings.Contains(image, "@sha256:") { return corev1.PullIfNotPresent }
    … if tag == "" || tag == defaultLatestTag { return corev1.PullAlways }
    return corev1.PullIfNotPresent
}
```

只有 `:latest`/无 tag 才会 `Always`。`…endpoint-probe:1.0.0` 走 `IfNotPresent`。

**实测步骤与输出**：

```powershell
# 1) 用 tester 目录当构建上下文，打**清单里那个**名字
cd testers/endpoint-probe
docker build -f build/Dockerfile -t registry.dev.huawei.com/testplane/endpoint-probe:1.0.0 .
# → config sha256:1390b87699f094f05b29f8dda57fda2c0c2010e8f4ee51c0d2d0da4cf0d2e839
#   与 node 里的 1390b87699f09 **完全一致**（构建可复现）

# 2) 二段式投递：先落宿主机文件，再用 cmd 的二进制安全管道灌进节点
docker save registry.dev.huawei.com/testplane/endpoint-probe:1.0.0 -o probe.tar      # 7,627,264 B
cmd /c "type probe.tar | docker exec -i testplane-control-plane sh -c \"cat > /tmp/probe.tar\""
docker exec testplane-control-plane sh -c "ctr -n k8s.io images import /tmp/probe.tar"
# → …sha256:c24777ba…   Importing elapsed: 0.4s
```

（`type |` 走的是 cmd，二进制安全；节点侧 `/tmp/probe.tar` 7,627,264 B 与宿主机**逐字节相等**。
本机 `docker cp` 会静默失败，确实不能用。）

```powershell
# 3) 决定性验证：造一个 Pod，imagePullPolicy=IfNotPresent，看它到底拉不拉
kubectl -n testplane run … --image-pull-policy=IfNotPresent …
# → 成功，日志：
#   uid=10001(tester) gid=10001(tester) groups=10001(tester)
#   /bin/bash  /usr/bin/jq  /usr/bin/curl  PROBE_OK
#   事件：Container image "…endpoint-probe:1.0.0" already present on machine
```

**`already present on machine` 就是答案：不拉远端，直接用节点里的。**

⚠️ 两个坑（都实测踩到了）：

1. **`testplane` namespace 开了 PodSecurity `restricted:latest`**。直接 `kubectl run` 会被拒：
   `violates PodSecurity "restricted:latest": allowPrivilegeEscalation != false … capabilities.drop=["ALL"] … runAsNonRoot != true … seccompProfile`。
   手工验证必须抄 `render.go` 那套 securityContext（`runAsNonRoot` / `10001:10001` / `fsGroup` /
   `RuntimeDefault` / `drop: ALL` / `readOnlyRootFilesystem`）。**这正是 `render.go:182-213` 写死这些字段的原因**。
2. 镜像是 `ENTRYPOINT ["/tester/run.sh"]`，验证要用 `--command` 覆盖，否则它去找 `--url` 会自己退出。

**结论：#4 的结论是「不需要再 `precheck: off`」**——镜像已在节点里、策略是 `IfNotPresent`、
Pod 能起来。只要有人（CI 或本机脚本）在 `kind create cluster` 后跑一次上面的 save→import，
`precheck: auto` 就能用。**这个导入步骤必须进 runbook**，否则重建集群后又退化成 `precheck: off`。

---

## 5. 真实日志体积（给 #52 的数据）

### 5.1 真跑设定

镜像 `ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.2.0`（= 官方 0330 镜像 +
`testers/aisbench-sla` 的 run.sh/tool + 烤进去的 tokenizer），目标 = kind 里 `testplane-mock/sla-mock`
（`mock_vllm.py --model-name mock-llm --port 18080`，经 `kubectl port-forward svc/sla-mock 18099:18080`
到宿主，容器侧用 `host.docker.internal:18099`）。

参数：`--input_len=64 --output_len=4 --data_num=4 --start=1 --max=1 --max_probes=1 --request_rate=0`，
数据集 `/tester/fixtures/smoke_dataset.jsonl`，`--work_path=/tmp/aisbench-ws`。

> 注：`--dataset` 给了现成文件时 `data_num` **不生效**——`sla_tool` 直接把文件软链过去，
> 唯一的 `write_data(..., num)` 截断逻辑只在生成路径里用
> （[`sla_tool/dataset.py:93-100`](../../testers/aisbench-sla/tool/sla_tool/dataset.py)）。
> 实测证据：fixture 是 **8 行**，`data_num=4`，而 ais_bench 日志写
> `Starting progress bar Total data num: 8`，CSV 里 `请求数=8`。**这是 #52 要注意的口径陷阱**。

### 5.2 体积（**实测**，单点 / 8 请求）

| 文件 | 字节 | 行数 | 说明 |
|---|---|---|---|
| `sla_out/points/c1/full.log` | **15,137** | **108** | 该点 ais_bench 的 stdout+stderr（ANSI 已剥） |
| `sla_out/aisbench_all.log` | **17,839** | **129** | 全 Run 聚合；实测 `full.log` 内容是它的**真子串**（`Contains` = True），头部多 ~2.7KB 的横幅/配置块 |
| **容器 stdout** | **18,290** | **137** | `--no_stream=false`（默认实时透传）时 |
| 容器 stderr | 624 | 6 | run.sh 的 `log()` 走 stderr |
| `sla_out/points/c1/20261106_035807/aisbench_full.log` | 15,137 | 108 | ais_bench **自己**在工作目录里写的同名日志，与本点 full.log **同尺寸**（同一份 stdout 的两次落盘，重复） |
| `logs/sla.log` | 4,392 | 37 | run.sh 的执行日志 |
| `result.json` | 5,128 | 157 | 契约产物 |

**单点产物总量**：整个 `OUT` 目录 **14 文件 / 102,160 B ≈ 99.8KB**，其中单点 work dir
8 文件 / 67,548 B ≈ 66.0KB（含 `gsm8k_plot.html` 13,283 B、`db_data/*.db` 12,288 B 这类
与日志无关的 ais_bench 原生产物）。

**换算**：约 **1.9KB / 12.8 行 每请求**（15,137B / 8 请求）。`data_num=64` 的点 ≈ 120KB，
一次 `max_probes=20` 的搜索 ≈ **2.4MB 日志**（20×120KB），加上 aisbench 自己的副本 ≈ 4.8MB。
`run.sh` 的 50MB 截断阈值（[`run.sh:182`](../../testers/aisbench-sla/run.sh) `MAX_ARTIFACT_BYTES`）
在常规参数下**不会触发**，但 `data_num=512` + 20 点这种组合会摸到。

**ANSI**：三个日志**都不含 ESC**（实测 `hasAnsi=False`）。因为 `run.sh` 传了 `--no-color`，
`aisbench.py` 的 tee 又对所有行做 `console.strip_ansi`
（[`aisbench.py:208-221`](../../testers/aisbench-sla/tool/sla_tool/aisbench.py)）。
→ #52 可以按纯文本渲染，不用考虑颜色转义。

### 5.3 给 #52 的三条结论

1. **呈现层只需要一个入口**：`sla_out/points/*/full.log` 是内容主体，`aisbench_all.log` 是它的
   超集（含横幅）。UI 展示「当前点在跑什么」用 stdout 实时流最省，事后溯源看 `full.log`。
2. **别把 ais_bench 自己写的 `aisbench_full.log` 再列一次**：它与 `full.log` 同尺寸、同内容，
   列出来只会让产物列表翻倍。
3. **`result.json.artifacts` 现在会列出 12 个 entry**（`run1` 实测：`metrics=5 artifacts=12`），
   其中含 `gsm8k_plot.html` / `.db` 这类二进制——按「日志」口径呈现时要先按扩展名过滤。

---

## 6. 未解问题 / 风险

| # | 问题 | 现状 |
|---|---|---|
| U1 | **规格 vs 实现不一致（挂载）** | `ARCHITECTURE §10.4`（`:1228`、`:1717`、`:1753`）说 `mounts: [{name,path,required}]` + `MOUNT_UNAVAILABLE`；实现是 `{name,mount_path,read_only,source}`，无 `required`、无该错误码。**必须有一边改**。 |
| U2 | **kind 里没跑过「真 ais_bench」Job** | 本文的真跑全是 `docker run`（用 `--read-only --tmpfs /tmp` 等价复现了 `ReadOnlyRootFilesystem` + `/tmp` emptyDir，**实测通过**），但 6.43GB 的 0.2.0 镜像**没导入 kind**（只导了 probe.tar 验证导入路径）。平台上端到端没验。**推断**：能跑，因为 ① uid/只读根/`/tmp` 都已等价复现 ② 路径只写 `/tmp` 和 `/workspace/output`。 |
| U3 | **`precheck: auto` 全链路没重跑** | 镜像在节点里、Pod 能起（实测），但平台侧的 `precheck` Task 没跑过。 |
| U4 | **mock 的保真度** | HTTP/1.0 无 keep-alive、token 用 chars/4 估。**兼容性没问题，性能结论不可用**。要不要给 mock 加 keep-alive / 真 tokenizer，是 #52 之外的另一个决定。 |
| U5 | **`data_num` 与 `--dataset` 的口径冲突** | 实测 `data_num=4` 却发了 8 请求。要么在 `tester.yaml` 的 `data_num` 描述里写清「给 `--dataset` 时本参数不生效」，要么在工具侧裁剪。 |
| U6 | **`_get_service_model_path` 无返回分支** | `镜像:base_api.py:130-147` 若 `status_code != 200` 走 `raise_for_status()` 然后**没有 `return`**（函数尾隐式返回 `None`）→ 空 model 会传给服务端。只在 `model` 为空时可达，属上游 bug，本 Tester 不触发。 |
| U7 | **0630 镜像的 transformers 5.x** | 未验证 `AutoTokenizer` + `default_perf` 在 transformers 5.12.1 下是否仍工作（本文全部基于 0330 / transformers 4.57.6）。**建议继续钉 0330**。 |

---

## 7. 复现命令

```powershell
# 0) 前置：kind 节点名 + mock 端口
kubectl -n testplane-mock port-forward svc/sla-mock 18099:18080     # 另开一个窗口

# 1) 真跑（等价平台 Job：只读根 + /tmp emptyDir + uid 10001）
docker run --rm --read-only --tmpfs /tmp:rw,size=1g `
  --add-host=host.docker.internal:host-gateway `
  -e OUTPUT_DIR=/out -v "<宿主输出目录>:/out" `
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.2.0 `
  --url=http://host.docker.internal:18099/v1 `
  --model_name=mock-llm --model_path=/models/Qwen3-0.6B `
  --input_len=64 --output_len=4 --data_num=4 `
  --sla=ttft.avg:100000 --start=1 --max=1 --max_probes=1 --request_rate=0 `
  --work_path=/tmp/aisbench-ws --dataset=/tester/fixtures/smoke_dataset.jsonl

# 2) tokenizer 走挂载（等价 §3 方案 ②）：把 /models 换成只读 bind mount
#    -v "D:\Models\Qwen3-0.6B:/opt/models/Qwen3-0.6B:ro"  →  --model_path=/opt/models/Qwen3-0.6B

# 3) 镜像导入 kind（#4 的标准动作）
docker save <img> -o x.tar
cmd /c "type x.tar | docker exec -i testplane-control-plane sh -c \"cat > /tmp/x.tar\""
docker exec testplane-control-plane sh -c "ctr -n k8s.io images import /tmp/x.tar"
```

本次运行产物（不入库）：`.data/aisbench-research/` 下的 `run1/`（无流式）、`run2/`（流式，量
stdout）、`run3/`（只读根）、`run4-mount/`（tokenizer 走挂载）、`run5-badtok/`（坏 tokenizer 的
错误形态）、`ep-probe-test.yaml`。
