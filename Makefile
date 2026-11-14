SHELL := /bin/bash
BIN := bin
CONFIG ?= config.dev.yaml
GO ?= go

.PHONY: help build test test-integration lint run migrate doctor seed demo up down reset smoke image clean fmt

help:
	@echo "GoDW 常用命令"
	@echo "  make build     编译 server / worker / cli 到 bin/"
	@echo "  make test      单元测试（含 -race）"
	@echo "  make test-integration  真实引擎集成测试（起 pg/mysql/ch，验建表与查询）"
	@echo "  make lint      go vet + gofmt 检查"
	@echo "  make run       本地启动控制面（需先 make up && make migrate）"
	@echo "  make migrate   执行数据库迁移"
	@echo "  make doctor    自检：配置 / 数据库 / 连接器 / 数据源"
	@echo "  make seed      灌入样例订单数据与模型/管道定义"
	@echo "  make demo      一键演示：灌数 + 三条管道 + 三个看板（幂等）"
	@echo "  make up        docker compose 起依赖（postgres/redis/mysql/clickhouse/minio）"
	@echo "  make down      停止并保留数据卷"
	@echo "  make reset     停止并删除数据卷（危险）"

build:
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(BIN)/godw-server ./cmd/godw-server
	$(GO) build -trimpath -o $(BIN)/godw-worker ./cmd/godw-worker
	$(GO) build -trimpath -o $(BIN)/godw-cli ./cmd/godw-cli
	@echo "构建完成: $(BIN)/"

test:
	$(GO) test -race -count=1 ./...

# 真实引擎集成测试：会起 postgres/mysql/clickhouse，验完即止不会清理数据卷。
test-integration:
	bash scripts/integration.sh

lint:
	$(GO) vet ./...
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi
	@echo "lint 通过"

fmt:
	gofmt -w cmd internal

run: build
	GODW_DB_PASSWORD=$${GODW_DB_PASSWORD:-godw} \
	GODW_AUTH_JWT_SECRET=$${GODW_AUTH_JWT_SECRET:-dev-secret-change-me} \
	$(BIN)/godw-server --config $(CONFIG)

migrate: build
	GODW_DB_PASSWORD=$${GODW_DB_PASSWORD:-godw} \
	GODW_AUTH_JWT_SECRET=$${GODW_AUTH_JWT_SECRET:-dev-secret-change-me} \
	$(BIN)/godw-cli migrate --config $(CONFIG)

doctor: build
	GODW_DB_PASSWORD=$${GODW_DB_PASSWORD:-godw} \
	GODW_AUTH_JWT_SECRET=$${GODW_AUTH_JWT_SECRET:-dev-secret-change-me} \
	$(BIN)/godw-cli doctor --config $(CONFIG)

seed: build
	bash scripts/seed.sh $(CONFIG)

demo: build
	bash scripts/demo.sh $(CONFIG)

smoke:
	bash scripts/smoke.sh

up:
	docker compose -f deploy/docker-compose.dev.yaml up -d
	@echo "等待 postgres 就绪…"
	@for i in $$(seq 1 30); do \
		docker compose -f deploy/docker-compose.dev.yaml exec -T postgres pg_isready -U godw -d godw >/dev/null 2>&1 && break; \
		sleep 1; \
	done

down:
	docker compose -f deploy/docker-compose.dev.yaml down

reset:
	docker compose -f deploy/docker-compose.dev.yaml down -v

image:
	docker build -f deploy/Dockerfile -t godw:dev .

clean:
	rm -rf $(BIN) data
