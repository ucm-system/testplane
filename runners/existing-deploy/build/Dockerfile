# existing-deploy · 内置 deploy 变体（已有服务登记）
# 构建上下文 = runners/existing-deploy（与发版矩阵一致）：
#   docker build -f runners/existing-deploy/build/Dockerfile -t registry.dev.huawei.com/argus/existing-deploy:1.0.0 runners/existing-deploy
FROM alpine:3.20
RUN apk add --no-cache bash curl jq ca-certificates tzdata \
 && addgroup -g 10001 argus && adduser -D -u 10001 -G argus -s /sbin/nologin argus \
 && mkdir -p /runner /workspace/input /workspace/output /tmp \
 && chown -R 10001:10001 /runner /workspace /tmp
COPY run.sh /runner/run.sh
RUN chmod +x /runner/run.sh && chown 10001:10001 /runner/run.sh
USER 10001:10001
WORKDIR /workspace/output
ENV TZ=Asia/Shanghai HOME=/tmp
ENTRYPOINT ["/runner/run.sh"]
