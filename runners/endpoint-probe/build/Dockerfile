# Argus Runner: endpoint-probe —— 镜像构建定义
#
# 只提供最小运行依赖：bash / curl / jq / CA 证书。
# 平台调用唯一入口 /runner/run.sh（ARCHITECTURE §5.2，runner.yaml 的 entry 数组指向它）。
#
# 构建上下文 = Runner 目录（不是本文件所在目录）：
#   cd runners/endpoint-probe   （本仓库当前仍为 runners/，目录改名随 v2 一并推进）
#   TAG=1.0.0
#   docker build -f build/Dockerfile -t registry.dev.huawei.com/argus/endpoint-probe:${TAG} .
#   docker push registry.dev.huawei.com/argus/endpoint-probe:${TAG}
# 构建由 CI 执行（§6.2）；runner.yaml 的 image 写产物引用即可 ——
# 平台不限制形态，tag 或 digest 都行（§5.6）。
#
# 这里刻意不放 build 脚本：构建只有两条命令，写在这里就够了。
#
# 本地一键构建 + 运行见 ../run-local.sh（不是契约入口，仅方便独立运行调试）。

# 建议钉 digest：FROM alpine:3.20@sha256:<digest>
FROM alpine:3.20

RUN apk add --no-cache \
        bash \
        curl \
        jq \
        ca-certificates \
        tzdata \
 && addgroup -g 10001 runner \
 && adduser  -D -u 10001 -G runner -s /sbin/nologin runner \
 && mkdir -p /workspace/input /workspace/output /runner \
 && chown -R 10001:10001 /workspace /runner

# 相对于构建上下文（= Runner 根目录）
COPY run.sh /runner/run.sh
RUN chmod 0755 /runner/run.sh

# 非 root 运行。挂载目录属主需由挂载方保证：
#   平台 K8s Executor -> securityContext.fsGroup: 10001
#   自己 docker run    -> -u "$(id -u):$(id -g)" 或 chown
USER 10001:10001

WORKDIR /workspace
ENV TZ=Asia/Shanghai

# 与 runner.yaml 的 entry 数组一致（唯一权威是 runner.yaml，§5.2）。
ENTRYPOINT ["/runner/run.sh"]
