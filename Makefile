# Argus 构建入口。规格见 design/views/05-development-view.md §5.1。
#
# Windows 上没有 make 时，用 deploy/README.md 里的等价 pwsh 命令；
# 目标名与语义保持一致，别在脚本里发明第二套。

SHELL   := /bin/bash
BIN     := bin
DSN     ?= postgres://argus:argus@127.0.0.1:5432/argus?sslmode=disable
PG_CTR  ?= argus-pg
KIND    ?= kind-argus

.PHONY: help build web server sync-spec migrate dev-db dev test test-race vet lint fmt fmt-check tidy openapi-lint clean

help:
	@echo "make build        前端构建 + 后端编译（产物 bin/argusd）"
	@echo "make web          pnpm 构建前端并复制到 server/internal/webui/dist（go:embed 不能跨目录）"
	@echo "make sync-spec    把 server/api/openapi.yaml 同步到 web/public/openapi.yaml（web 构建前自动跑）"
	@echo "make server       CGO_ENABLED=0 编译 bin/argusd"
	@echo "make test         go test ./..."
	@echo "make test-race    go test -race ./..."
	@echo "make lint         go vet + openapi lint"
	@echo "make migrate      对 $(PG_CTR) 应用 server/migrations/*.sql（只前向）"
	@echo "make dev-db       起本地 PG（docker）"
	@echo "make dev          起全栈（PG + 迁移 + --role=all）"
	@echo "make fmt          gofmt 全部 Go 文件（按 LF 形态判定，绕开 Windows CRLF）"`n	@echo "make fmt-check    只检查不修改（CI 门禁用）"`n	@echo "make clean        清构建产物（webui/dist 只留占位 index.html）"

build: web server

web: sync-spec
	pnpm --dir web install --frozen-lockfile=false
	pnpm --dir web build
	rm -rf server/internal/webui/dist && cp -r web/dist server/internal/webui/dist

server:
	cd server && CGO_ENABLED=0 go build -trimpath -o ../$(BIN)/argusd ./cmd/argusd

# web/public/openapi.yaml 是契约的静态分发副本（/openapi.yaml 与 /llms.txt 一起服务）；
# 真源是 server/api/openapi.yaml，构建前端前强制同步，避免两个文件漂移。
sync-spec:
	cp server/api/openapi.yaml web/public/openapi.yaml

test:
	cd server && go test ./...

test-race:
	cd server && go test -race ./...

fmt:
	pwsh scripts/gofmt-check.ps1 -Fix

fmt-check:
	pwsh scripts/gofmt-check.ps1

vet:
	cd server && go vet ./...

lint: vet openapi-lint

tidy:
	cd server && go mod tidy

openapi-lint:
	npx --yes @redocly/cli@latest lint server/api/openapi.yaml

migrate:
	@for f in server/migrations/*.sql; do \
		echo "applying $$f"; \
		docker exec -i $(PG_CTR) psql -U argus -d argus -v ON_ERROR_STOP=1 < "$$f" || exit 1; \
	done

dev-db:
	docker run --rm -d --name $(PG_CTR) \
		-e POSTGRES_USER=argus -e POSTGRES_PASSWORD=argus -e POSTGRES_DB=argus \
		-p 5432:5432 postgres:16 || true

dev: dev-db migrate
	cd server && AG_ROLE=all AG_DB_DSN='$(DSN)' go run ./cmd/argusd --role=all

clean:
	rm -rf bin web/dist
	# 保留占位 index.html（//go:embed all:dist 依赖它），但把 make web 复制进来的 assets 等产物删干净，
	# 否则 clean 之后编出来的二进制里仍然带着上次的前端产物（issue #24-5）。不要用 git checkout 恢复——那会丢掉本地改动。
	rm -rf server/internal/webui/dist/assets server/internal/webui/dist/*.js server/internal/webui/dist/*.css server/internal/webui/dist/*.map 2>/dev/null || true
	@test -f server/internal/webui/dist/index.html || printf '<!doctype html><title>Argus placeholder</title>\n<p>前端未构建：跑 make web 生成。</p>\n' > server/internal/webui/dist/index.html

# ---- 镜像与 Helm（生产部署路径）------------------------------------------------
# 构建镜像并（可选）推到 registry；digest 就是 chart 里 image.digest 要填的值。
image:
	pwsh deploy/docker/build-push.ps1

image-push:
	pwsh deploy/docker/build-push.ps1 -Push

# chart 静态校验 + 渲染（生产模板；缺必填项会直接报错，这是有意的）
helm-lint:
	helm lint deploy/charts/argus -f deploy/charts/argus/values-production.example.yaml

helm-template:
	helm template argus deploy/charts/argus -n argus \
		-f deploy/charts/argus/values-production.example.yaml

# 本地 kind：构建镜像灌进节点 + 用 values-kind.yaml 装（dev 鉴权 + mock 部署方）
helm-install-local:
	pwsh deploy/docker/build-push.ps1 -KindCluster argus -Tag kind-local
	helm upgrade --install argus deploy/charts/argus -n argus-helm --create-namespace \
		-f deploy/charts/argus/values-kind.yaml --set image.tag=kind-local --wait --timeout 10m

helm-uninstall-local:
	helm uninstall argus -n argus-helm
