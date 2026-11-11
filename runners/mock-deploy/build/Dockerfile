# Argus 内置 Runner: mock-deploy —— mock 服务接入的镜像构建定义
#
# v1.1.0 起与 genet-deploy 分道（D-75）：独立 run.sh（只做 mock 服务就绪探测 +
# 四件套导出，不调 Genet OpenAPI、不创建集群对象）。构建配方与 genet-deploy 相同
# （alpine + bash/curl/jq，非 root）。
#
# 构建上下文 = Runner 目录（不是本文件所在目录）：
#   cd runners/mock-deploy
#   TAG=1.1.0
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
