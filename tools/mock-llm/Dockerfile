# TestPlane · mock-llm 镜像构建定义
#
# 本地/Kind 用（CI 不需要发布这个镜像——它只服务于测试）：
#   docker build -t testplane/mock-llm:dev tools/mock-llm
#   kind load docker-image testplane/mock-llm:dev --name testplane
#
# 只用标准库，构建阶段不联网（没有 go.sum 依赖）。

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-llm .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 -s /sbin/nologin mock
COPY --from=build /out/mock-llm /usr/local/bin/mock-llm
USER 10001:10001
EXPOSE 18080
# 容器内监听所有地址；探针按 http://<service>:18080/v1 访问
ENTRYPOINT ["/usr/local/bin/mock-llm", "-addr=:18080"]
