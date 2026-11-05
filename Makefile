# TestPlane 构建入口。规格见 design/views/05-development-view.md §5.1。
#
# Windows 上没有 make 时，用 deploy/README.md 里的等价 pwsh 命令；
# 目标名与语义保持一致，别在脚本里发明第二套。

SHELL   := /bin/bash
BIN     := bin
DSN     ?= postgres://testplane:testplane@127.0.0.1:5432/testplane?sslmode=disable
PG_CTR  ?= testplane-pg
KIND    ?= kind-testplane

.PHONY: help build web server migrate dev-db dev test test-race vet lint fmt tidy openapi-lint clean

help:
	@echo "make build        前端构建 + 后端编译（产物 bin/testplaned）"
	@echo "make web          pnpm 构建前端并复制到 server/internal/webui/dist（go:embed 不能跨目录）"
	@echo "make server       CGO_ENABLED=0 编译 bin/testplaned"
	@echo "make test         go test ./..."
	@echo "make test-race    go test -race ./..."
	@echo "make lint         go vet + openapi lint"
	@echo "make migrate      对 $(PG_CTR) 应用 server/migrations/*.sql（只前向）"
	@echo "make dev-db       起本地 PG（docker）"
	@echo "make dev          起全栈（PG + 迁移 + --role=all）"
	@echo "make clean        清构建产物"

build: web server

web:
	pnpm --dir web install --frozen-lockfile=false
	pnpm --dir web build
	rm -rf server/internal/webui/dist && cp -r web/dist server/internal/webui/dist

server:
	cd server && CGO_ENABLED=0 go build -trimpath -o ../$(BIN)/testplaned ./cmd/testplaned

test:
	cd server && go test ./...

test-race:
	cd server && go test -race ./...

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
		docker exec -i $(PG_CTR) psql -U testplane -d testplane -v ON_ERROR_STOP=1 < "$$f" || exit 1; \
	done

dev-db:
	docker run --rm -d --name $(PG_CTR) \
		-e POSTGRES_USER=testplane -e POSTGRES_PASSWORD=testplane -e POSTGRES_DB=testplane \
		-p 5432:5432 postgres:16 || true

dev: dev-db migrate
	cd server && TESTPLANE_ROLE=all TESTPLANE_DATABASE_URL='$(DSN)' go run ./cmd/testplaned --role=all

clean:
	rm -rf bin web/dist
	# 保留占位 index.html：webui 用 //go:embed all:dist，删掉目录会让 go build / make test 直接失败
	@git checkout -- server/internal/webui/dist/index.html 2>/dev/null || true
