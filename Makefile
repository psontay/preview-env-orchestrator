# Makefile — các tác vụ thường dùng cho preview-env-orchestrator.
#
#   make            # in danh sách target
#   make run        # chạy API server
#   make test       # unit test
#   make compose-up # dựng app + Postgres + Jaeger

.DEFAULT_GOAL := help

# Nạp biến từ .env nếu có (chủ yếu cho target `migrate`).
-include .env
export

# Giá trị mặc định khi .env chưa có / chưa đặt biến.
POSTGRES_USER ?= preview
POSTGRES_DB   ?= preview

.PHONY: help run build test lint compose-up compose-down migrate

help: ## In danh sách target
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-13s\033[0m %s\n", $$1, $$2}'

run: ## Chạy API server (go run ./cmd/api)
	go run ./cmd/api

build: ## Build binary tĩnh vào ./bin/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/api ./cmd/api

test: ## Chạy toàn bộ unit test
	go test ./...

lint: ## Kiểm tra gofmt + go vet
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt cần chạy trên:"; echo "$$out"; exit 1; fi
	go vet ./...

compose-up: ## Dựng app + Postgres + Jaeger (docker compose up -d --build)
	docker compose up -d --build

compose-down: ## Gỡ stack, giữ lại volume dữ liệu
	docker compose down

migrate: ## Áp dụng migrations/*.sql vào Postgres đang chạy trong compose
	@for f in $$(ls migrations/*.sql | sort); do \
		echo ">> applying $$f"; \
		docker compose exec -T postgres \
			psql -v ON_ERROR_STOP=1 -U "$(POSTGRES_USER)" -d "$(POSTGRES_DB)" < "$$f"; \
	done
