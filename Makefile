.DEFAULT_GOAL := help
COMPOSE := docker compose -f deployments/local/docker-compose.yml

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: deps-up deps-down
deps-up: ## Start local PostgreSQL, Redis, RabbitMQ
	$(COMPOSE) up -d --wait
deps-down: ## Stop local dependencies (keeps data volume)
	$(COMPOSE) down

.PHONY: generate
generate: ## Regenerate database code from internal/store/queries (sqlc v1.31.1)
	sqlc generate

.PHONY: migrate
migrate: ## Apply database migrations (DATABASE_URL from .env)
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/migrate up

.PHONY: build run-api
build: ## Build all binaries into ./bin
	go build -o bin/ ./cmd/...
run-api: ## Run the API with variables from .env
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/api

.PHONY: fmt fmt-check vet test test-integration vuln check
fmt: ## Format Go code
	gofmt -w .
fmt-check: ## Fail if any Go file is not gofmt-formatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi
vet: ## go vet
	go vet ./...
test: ## Unit tests with race detector
	go test -race -count=1 ./...
test-integration: ## Unit + integration tests (needs TEST_DATABASE_URL)
	go test -race -count=1 -tags=integration ./...
vuln: ## Scan dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
check: fmt-check vet test ## Everything CI runs except vuln
