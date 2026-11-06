.PHONY: help build run test clean frontend frontend-build frontend-copy docker-build-app docker-build-docreader docker-build-mcp-redmine docker-build-all docker-login docker-push publish docker-run migrate-up migrate-down docker-restart docker-stop start-all stop-all start-ollama stop-ollama clean-images check-env list-containers pull-images show-platform dev-start dev-stop dev-restart dev-logs dev-status dev-app dev-frontend docs install-swagger build-lite run-lite package-lite anydoc-lib build-anydoc

# Show help
help:
	@echo "WeKnora Makefile 帮助"
	@echo ""
	@echo "基础命令:"
	@echo "  build             构建应用"
	@echo "  run               运行应用"
	@echo "  test              运行测试"
	@echo "  anydoc-lib        构建 anydoc 静态库（需要 Rust 工具链）"
	@echo "  build-anydoc      构建带 anydoc 解析引擎的应用"
	@echo "  clean             清理构建文件"
	@echo ""
	@echo "发布 (Harbor: $(DOCKER_IMAGE)):"
	@echo "  frontend              构建前端并复制到 ./frontend/ (Go server 直接提供 Web 服务，无前端容器)"
	@echo "  frontend-build        仅编译前端 (npm run build)"
	@echo "  frontend-copy        仅复制前端产物到 ./frontend/"
	@echo "  docker-build-app      构建应用镜像 (Go server 同时提供 API 和前端)"
	@echo "  docker-login          登录 Harbor"
	@echo "  docker-push           推送应用镜像到 Harbor"
	@echo "  publish               完整发布: 前端构建+复制 → 镜像构建 → 推送 Harbor"
	@echo ""
	@echo "Docker 命令:"
	@echo "  docker-build-docreader 构建文档读取器镜像 ($(DOCKER_IMAGE_DOCREADER))"
	@echo "  docker-build-mcp-redmine 构建 Redmine MCP 镜像 ($(DOCKER_IMAGE_MCP_REDMINE))"
	@echo "  docker-build-all       构建所有 Docker 镜像 (app + docreader + mcp-redmine)"
	@echo "  clean-images           清理本地旧镜像（含已废弃的 wechatopenai 旧镜像）"
	@echo "  docker-run            运行 Docker 容器"
	@echo "  docker-stop           停止 Docker 容器"
	@echo "  docker-restart        重启 Docker 容器"
	@echo ""
	@echo "服务管理:"
	@echo "  start-all         启动所有服务"
	@echo "  stop-all          停止所有服务"
	@echo "  start-ollama      仅启动 Ollama 服务"
	@echo ""
	@echo "数据库:"
	@echo "  migrate-up        执行数据库迁移"
	@echo "  migrate-down      回滚数据库迁移"
	@echo ""
	@echo "开发工具:"
	@echo "  fmt               格式化代码"
	@echo "  lint              代码检查"
	@echo "  deps              安装依赖"
	@echo "  docs              生成 Swagger API 文档"
	@echo "  install-swagger   安装 swag 工具"
	@echo ""
	@echo "模型厂商目录:"
	@echo "  model-catalog-check   校验厂商目录（不变量 + 新旧行为对照 + 厂商测试）"
	@echo "  model-catalog-diff    对比 models.dev，输出模型元数据差异报告（需人工审阅）"
	@echo "                        可选: make model-catalog-diff VENDOR=deepseek"
	@echo ""
	@echo "环境检查:"
	@echo "  check-env         检查环境配置"
	@echo "  list-containers   列出运行中的容器"
	@echo "  pull-images       拉取最新镜像"
	@echo "  show-platform     显示当前构建平台"
	@echo ""
	@echo "开发模式（推荐）:"
	@echo "  dev-start         启动开发环境基础设施（仅启动依赖服务）"
	@echo "                    可选: make dev-start DEV_ARGS=--odl-hybrid"
	@echo "  dev-stop          停止开发环境"
	@echo "  dev-restart       重启开发环境"
	@echo "  dev-logs          查看开发环境日志"
	@echo "  dev-status        查看开发环境状态"
	@echo "  dev-app           启动后端应用（本地运行，需先运行 dev-start）"
	@echo "                    已 make anydoc-lib 时自动链接 anydoc 引擎"
	@echo "  dev-frontend      启动前端（本地运行，需先运行 dev-start）"
	@echo ""
	@echo "Lite 模式（零外部依赖）:"
	@echo "  build-lite        构建 Lite 版本（先构建前端到 web/，再构建 Go；SKIP_FRONTEND=1 跳过前端）"
	@echo "  run-lite          构建并启动 Lite 版本"
	@echo "  package-lite      构建并打包 Lite 发行包（tarball）"
	@echo "  package-mac-app   构建并打包 macOS 桌面应用 (.app)"

# Go related variables
BINARY_NAME=mindBuilder
MAIN_PATH=./cmd/server

# Docker related variables
# 镜像仓库: harbor.arfa.wise-paas.com，Harbor 工程(目录)为 mindbuilder，
# 与 docker-compose.yml 中 app 服务引用的镜像保持一致。
DOCKER_HOST=harbor.arfa.wise-paas.com
DOCKER_IMAGE=$(DOCKER_HOST)/mindbuilder/app
DOCKER_IMAGE_DOCREADER=$(DOCKER_HOST)/mindbuilder/docreader
DOCKER_IMAGE_MCP_REDMINE=$(DOCKER_HOST)/mindbuilder/mcp-redmine
DOCKER_TAG=v1.0.0

# 前端源码在独立仓库，构建产物复制到 ./frontend/，
# 由 Go server 直接提供 Web 服务（internal/router/static.go），
# 不再有独立的前端容器。
FRONTEND_SRC_DIR=/home/sgq/MYPROJECT/FRONTEND/mindBuilder
FRONTEND_DIST_DIR=$(FRONTEND_SRC_DIR)/dist

# Platform detection
ifeq ($(shell uname -m),x86_64)
    PLATFORM=linux/amd64
else ifeq ($(shell uname -m),aarch64)
    PLATFORM=linux/arm64
else ifeq ($(shell uname -m),arm64)
    PLATFORM=linux/arm64
else
    PLATFORM=linux/amd64
endif

# ---------------------------------------------------------------------------
# 前端构建（发布流程第一步）
# 前端源码在独立仓库 $(FRONTEND_SRC_DIR)，npm run build 产出 dist/，
# 复制到本仓库 ./frontend/ 后由 Go server 直接提供 Web 服务（无前端容器）。
# ---------------------------------------------------------------------------
frontend: frontend-build frontend-copy

frontend-build:
	@echo ">> 编译前端 ($(FRONTEND_SRC_DIR))..."
	cd $(FRONTEND_SRC_DIR) && \
		VITE_FRONTEND_COMMIT=$$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
		NODE_OPTIONS="--max-old-space-size=4096" \
		npm run build

frontend-copy:
	@test -f $(FRONTEND_DIST_DIR)/index.html || { \
		echo "错误: $(FRONTEND_DIST_DIR)/index.html 不存在，请先 make frontend-build"; \
		exit 1; \
	}
	@echo ">> 复制前端产物到 ./frontend/ ..."
	@rm -rf frontend
	@mkdir -p frontend
	@cp -r $(FRONTEND_DIST_DIR)/. frontend/
	@echo ">> 完成，Go server 将直接提供前端 Web 服务"

# Harbor 登录 / 推送
docker-login:
	docker login $(DOCKER_HOST)

docker-push:
	docker push $(DOCKER_IMAGE):$(DOCKER_TAG)

# 完整发布流程：前端构建+复制 → 应用镜像构建 → 推送 Harbor
publish: frontend docker-build-app docker-push
	@echo ">> 发布完成: $(DOCKER_IMAGE):$(DOCKER_TAG)"

# Build the application
build:
	go build -o $(BINARY_NAME) $(MAIN_PATH)

# Build the anydoc static archive (Rust) that the `anydoc` build tag links.
# Override the platform with TARGET=<rust-target-triple>.
anydoc-lib:
	./scripts/build-anydoc-lib.sh

# Build the application with the in-process anydoc parser engine linked in.
build-anydoc: anydoc-lib
	go build -tags anydoc -o $(BINARY_NAME) $(MAIN_PATH)

# Run the application
run: build
	./$(BINARY_NAME)

# Run tests
test:
	go test -v ./...

# Generate reviewed metadata + protocol overrides, then verify every model.
.PHONY: model-catalog-generate model-catalog-check
model-catalog-generate:
	python3 scripts/model-catalog/generate.py

model-catalog-check:
	python3 scripts/model-catalog/generate.py --check
	go test ./internal/models/...

# Vendor catalog: report where our model metadata differs from models.dev.
# Development aid only — nothing is fetched at runtime and nothing is written
# automatically; review each line against the vendor's own documentation.
.PHONY: model-catalog-diff
model-catalog-diff:
	@python3 scripts/model_catalog_diff.py $(if $(VENDOR),--vendor $(VENDOR),)

# Clean build artifacts
clean:
	go clean
	rm -f $(BINARY_NAME)

# Build Docker image (Go server 同时提供 API 和前端，前端产物打进镜像)
docker-build-app:
	@test -f frontend/index.html || { \
		echo "错误: frontend/index.html 不存在 — 先运行 make frontend (编译并复制前端产物)"; \
		exit 1; \
	}
	@echo "获取版本信息..."
	@eval $$(./scripts/get_version.sh env); \
	./scripts/get_version.sh info; \
	docker build --platform $(PLATFORM) \
		--build-arg VERSION_ARG="$$VERSION" \
		--build-arg COMMIT_ID_ARG="$$COMMIT_ID" \
		--build-arg BUILD_TIME_ARG="$$BUILD_TIME" \
		--build-arg GO_VERSION_ARG="$$GO_VERSION" \
		--build-arg WITH_ANYDOC=$${WITH_ANYDOC:-1} \
		--build-arg GOPROXY_ARG="$${GOPROXY_ARG:-https://goproxy.cn,direct}" \
		--build-arg GOPRIVATE_ARG="$${GOPRIVATE_ARG:-}" \
		--build-arg GOSUMDB_ARG="$${GOSUMDB_ARG:-off}" \
		-f docker/Dockerfile.app -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

# Build docreader Docker image
docker-build-docreader:
	docker build --platform $(PLATFORM) -f docker/Dockerfile.docreader -t $(DOCKER_IMAGE_DOCREADER):$(DOCKER_TAG) .

# Build mcp-redmine Docker image
docker-build-mcp-redmine:
	docker build --platform $(PLATFORM) -f docker/Dockerfile.mcp-redmine -t $(DOCKER_IMAGE_MCP_REDMINE):$(DOCKER_TAG) .

# Build all Docker images
docker-build-all: docker-build-app docker-build-docreader docker-build-mcp-redmine

# Run Docker container (传统方式)
# Touch .env if missing — docker-compose.yml's `env_file: [.env]` is required
# for ${ENV} interpolation in builtin_models.yaml and would otherwise refuse
# to parse on fresh clones. `start-all` handles this via check_env_file; this
# direct path needs its own guard.
docker-run:
	@[ -f .env ] || ([ -f .env.example ] && cp .env.example .env || touch .env)
	docker-compose up

# 使用新脚本启动所有服务
start-all:
	./scripts/start_all.sh

# 使用新脚本仅启动Ollama服务
start-ollama:
	./scripts/start_all.sh --ollama

# 使用新脚本仅启动Docker容器
start-docker:
	./scripts/start_all.sh --docker

# 使用新脚本停止所有服务
stop-all:
	./scripts/start_all.sh --stop

# Stop Docker container (传统方式)
docker-stop:
	docker-compose down

# 清理本地旧镜像（停掉并删除 wechatopenai 时代的旧容器/镜像，一次性迁移用）
clean-images:
	./scripts/build_images.sh --clean

# Restart Docker container (stop, start)
docker-restart:
	@[ -f .env ] || ([ -f .env.example ] && cp .env.example .env || touch .env)
	docker-compose stop -t 60
	docker-compose up

# Database migrations
migrate-up:
	./scripts/migrate.sh up

migrate-down:
	./scripts/migrate.sh down

migrate-version:
	./scripts/migrate.sh version

migrate-create:
	@if [ -z "$(name)" ]; then \
		echo "Error: migration name is required"; \
		echo "Usage: make migrate-create name=your_migration_name"; \
		exit 1; \
	fi
	./scripts/migrate.sh create $(name)

migrate-force:
	@if [ -z "$(version)" ]; then \
		echo "Error: version is required"; \
		echo "Usage: make migrate-force version=4"; \
		exit 1; \
	fi
	./scripts/migrate.sh force $(version)

migrate-goto:
	@if [ -z "$(version)" ]; then \
		echo "Error: version is required"; \
		echo "Usage: make migrate-goto version=3"; \
		exit 1; \
	fi
	./scripts/migrate.sh goto $(version)

# Generate API documentation (Swagger)
#
# internal/localsandbox is excluded because cmd/server does not depend on it
# (its importers are behind the `desktop` build tag). Without the exclude,
# swag walks into the directory, follows its `const X = core.X` aliases and
# loads internal/localsandbox/core a second time through go/loader; that
# second load re-registers every model under its short name without enum
# values, so definitions lose their package-qualified names and
# docs/swagger_contract_test.go fails.
docs:
	@echo "生成 Swagger API 文档..."
	swag init -g $(MAIN_PATH)/main.go -o ./docs --parseDependency --parseInternal \
		--exclude ./internal/localsandbox --overridesFile ./docs/swag.overrides
	@echo "文档已生成到 ./docs 目录"
	@echo "启动服务后访问 http://localhost:8080/swagger/index.html 查看文档"

# Install swagger tool
install-swagger:
	go install github.com/swaggo/swag/cmd/swag@latest

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Install dependencies
deps:
	go mod download

# Build for production
# google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn for qdrant milvus proto conflict
# GO_BUILD_TAGS adds optional build tags, e.g. GO_BUILD_TAGS=anydoc to link the
# in-process office document parser (run `make anydoc-lib` first).
build-prod:
	VERSION=$$(git describe --tags --abbrev=0 2>/dev/null || echo "$${VERSION:-unknown}"); \
	COMMIT_ID=$${COMMIT_ID:-unknown}; \
	CGO_ENABLED=1 \
	CGO_CFLAGS="-Wno-deprecated-declarations" \
	CGO_LDFLAGS="$$(if [ "$$(uname)" = 'Darwin' ]; then echo '-Wl,-no_warn_duplicate_libraries'; fi)" \
	BUILD_TIME=$${BUILD_TIME:-unknown}; \
	GO_VERSION=$${GO_VERSION:-unknown}; \
	LDFLAGS="-X 'github.com/Tencent/WeKnora/internal/handler.Version=$$VERSION' -X 'github.com/Tencent/WeKnora/internal/handler.Edition=standard' -X 'github.com/Tencent/WeKnora/internal/handler.CommitID=$$COMMIT_ID' -X 'github.com/Tencent/WeKnora/internal/handler.BuildTime=$$BUILD_TIME' -X 'github.com/Tencent/WeKnora/internal/handler.GoVersion=$$GO_VERSION' -X 'google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn'"; \
	go build -tags "$(GO_BUILD_TAGS)" -ldflags="-w -s $$LDFLAGS" -o $(BINARY_NAME) $(MAIN_PATH)

# Build Lite version (single binary, SQLite + in-memory queue)
# 会先构建前端到 web/，再构建 Go 二进制；SKIP_FRONTEND=1 可跳过前端
build-lite:
	@if [ -f frontend/package.json ] && [ "$${SKIP_FRONTEND:-}" != "1" ]; then \
		echo ">> Building frontend for Lite..."; \
		(cd frontend && npm ci --prefer-offline && npm run build) && \
		rm -rf web && cp -r frontend/dist web; \
	elif [ "$${SKIP_FRONTEND:-}" = "1" ]; then \
		echo ">> Skipping frontend (SKIP_FRONTEND=1)"; \
	else \
		echo ">> No frontend/package.json, skipping frontend"; \
	fi
	export EDITION=lite; \
	eval "$$(./scripts/get_version.sh env)"; \
	LDFLAGS="$$(./scripts/get_version.sh ldflags) -X 'google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn'"; \
	CGO_ENABLED=1 \
	CGO_CFLAGS="-Wno-deprecated-declarations" \
	CGO_LDFLAGS="$$(if [ "$$(uname)" = 'Darwin' ]; then echo '-Wl,-no_warn_duplicate_libraries'; fi)" \
	go build -tags "sqlite_fts5" -ldflags="-w -s $$LDFLAGS" -o $(BINARY_NAME)-lite $(MAIN_PATH)

# Run Lite version with .env.lite defaults
run-lite: build-lite
	@if [ ! -f .env.lite ]; then echo "Error: .env.lite not found"; exit 1; fi
	@set -a && . ./.env.lite && set +a && ./$(BINARY_NAME)-lite

# Package Lite version into distributable tarball
package-lite:
	./scripts/package-lite.sh

# Package Mac App
package-mac-app:
	./scripts/package-mac-app.sh

download_spatial:
	go run cmd/download/duckdb/duckdb.go

clean-db:
	@echo "Cleaning database..."
	@if [ $$(docker volume ls -q -f name=weknora_postgres-data) ]; then \
		docker volume rm weknora_postgres-data; \
	fi
	@if [ $$(docker volume ls -q -f name=weknora_minio_data) ]; then \
		docker volume rm weknora_minio_data; \
	fi
	@if [ $$(docker volume ls -q -f name=weknora_redis_data) ]; then \
		docker volume rm weknora_redis_data; \
	fi

# Environment check
check-env:
	./scripts/start_all.sh --check

# List containers
list-containers:
	./scripts/start_all.sh --list

# Pull latest images
pull-images:
	./scripts/start_all.sh --pull

# Show current platform
show-platform:
	@echo "当前系统架构: $(shell uname -m)"
	@echo "Docker构建平台: $(PLATFORM)"

# Development mode commands
dev-start:
	./scripts/dev.sh start $(DEV_ARGS)

dev-stop:
	./scripts/dev.sh stop

dev-restart:
	./scripts/dev.sh restart

dev-logs:
	./scripts/dev.sh logs

dev-status:
	./scripts/dev.sh status

dev-app:
	./scripts/dev.sh app

dev-frontend:
	./scripts/dev.sh frontend
