# Argus 内置 Runner: service-clean —— 镜像构建定义
#
# 只提供最小运行依赖：bash / curl / jq / CA 证书（与 Genet OpenAPI 交互只需这些）。
# 平台调用唯一入口 /runner/run.sh（ARCHITECTURE §5.2，runner.yaml 的 entry 数组指向它）。
#
# 构建上下文 = Runner 目录（不是本文件所在目录）：
#   cd runners/service-clean
#   TAG=1.0.0
#   docker build -f build/Dockerfile -t registry.dev.huawei.com/argus/service-clean:${TAG} .
#   docker push registry.dev.huawei.com/argus/service-clean:${TAG}
# 本地联调 tag：docker build -f build/Dockerfile -t argus-service-clean:dev .
# 构建由 CI 执行（§6.2）；runner.yaml 的 image 写产物引用即可（tag / digest 均可，§5.6）。
#
# APK_MIRROR 仅是网络环境旋钮（默认官方 CDN，等于不改源）：
#   docker build --build-arg APK_MIRROR=https://mirrors.tuna.tsinghua.edu.cn/alpine ...

ARG APK_MIRROR=https://dl-cdn.alpinelinux.org/alpine
FROM alpine:3.20
ARG APK_MIRROR

RUN sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${APK_MIRROR}|g" /etc/apk/repositories \
 && apk add --no-cache \
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
