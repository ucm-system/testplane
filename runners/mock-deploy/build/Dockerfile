# Argus 内置 Runner: mock-deploy —— mock 部署薄壳的镜像构建定义
#
# 与 genet-deploy **同一份 run.sh / 同一构建配方**（薄壳裁决：mock = genet-deploy +
# 接入点指向 fake-genet，OpenAPI 契约一致，零行为分叉）。差异只有两点：
#   1. runner.yaml 的 name/image 不同；
#   2. runner.yaml 的 env 为 AG_GENET_BASE_URL 声明了 default（fake-genet 接入点），
#      平台按 runner.yaml env 声明注入，不再按 provider 特判（deploy/clean 解耦）。
#
# 构建上下文 = Runner 目录（不是本文件所在目录）：
#   cd runners/mock-deploy
#   TAG=1.0.0
#   docker build -f build/Dockerfile -t registry.dev.huawei.com/argus/mock-deploy:${TAG} .
#   docker push registry.dev.huawei.com/argus/mock-deploy:${TAG}
# 本地联调 tag：docker build -f build/Dockerfile -t argus-mock-deploy:dev .
#
# APK_MIRROR 仅是网络环境旋钮（默认官方 CDN，等于不改源）：
#   docker build --build-arg APK_MIRROR=https://mirrors.tuna.tsinghua.edu.cn/alpine ...
# CI（内网/镜像源可达时）无需传它。

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
