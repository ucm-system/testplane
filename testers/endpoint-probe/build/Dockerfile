# TestPlane Tester: endpoint-probe —— 镜像构建定义
#
# 只提供最小运行依赖：bash / curl / jq / CA 证书。
# 平台调用唯一入口 /tester/run.sh（ARCHITECTURE §5.2 Single Entry Point）。
#
# 构建上下文 = Tester 目录（不是本文件所在目录）：
#   cd testers/endpoint-probe
#   TAG=1.0.0
#   docker build -f build/Dockerfile -t registry.dev.huawei.com/testplane/endpoint-probe:${TAG} .
#   docker push registry.dev.huawei.com/testplane/endpoint-probe:${TAG}
# 构建由 CI 执行（§6.2）；tester.yaml 的 image 写产物引用即可 ——
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
 && addgroup -g 10001 tester \
 && adduser  -D -u 10001 -G tester -s /sbin/nologin tester \
 && mkdir -p /workspace/input /workspace/output /tester \
 && chown -R 10001:10001 /workspace /tester

# 相对于构建上下文（= Tester 根目录）
COPY run.sh /tester/run.sh
RUN chmod 0755 /tester/run.sh

# 非 root 运行（§13.2）。挂载目录属主需由挂载方保证：
#   平台 K8s Executor -> securityContext.fsGroup: 10001
#   自己 docker run    -> -u "$(id -u):$(id -g)" 或 chown
USER 10001:10001

WORKDIR /workspace
ENV TZ=Asia/Shanghai

# 平台 Job Spec 的 command 也是 /tester/run.sh，二者等价。
ENTRYPOINT ["/tester/run.sh"]
