# Makefile — the handful of commands you'll actually use. Run `make help` for the list.

# Local infra connection details (match docker-compose.yml).
DATABASE_URL ?= postgres://collab:collab@localhost:5432/collab
REDIS_ADDR   ?= localhost:6379

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: run
run: ## Run the server locally (uses local Postgres+Redis if up; else in-memory)
	DATABASE_URL="$(DATABASE_URL)" REDIS_ADDR="$(REDIS_ADDR)" go run ./cmd/server

.PHONY: run-mem
run-mem: ## Run with no database/Redis (pure in-memory, quickest demo)
	go run ./cmd/server

.PHONY: build
build: ## Build the server binary into ./bin/server
	go build -o bin/server ./cmd/server

.PHONY: test
test: ## Run unit tests (no external services needed)
	go test ./...

.PHONY: test-integration
test-integration: ## Run ALL tests incl. Postgres+Redis integration (needs infra up)
	TEST_DATABASE_URL="$(DATABASE_URL)" TEST_REDIS_ADDR="$(REDIS_ADDR)" go test -race ./...

.PHONY: test-crdt
test-crdt: ## Run just the CRDT tests, verbosely (incl. randomized convergence)
	go test ./internal/crdt -v

.PHONY: loadtest
loadtest: ## Run the in-process convergence/latency load test harness
	go test ./internal/crdt -run TestLoad -v

.PHONY: fmt
fmt: ## gofmt the whole tree
	gofmt -w .

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: infra-up
infra-up: ## Start Postgres + Redis via docker compose
	docker compose up -d db redis

.PHONY: infra-down
infra-down: ## Stop Postgres + Redis (keeps data volume)
	docker compose stop db redis

.PHONY: up
up: ## Build & run the full stack (app + db + redis) on :8080
	docker compose up --build

.PHONY: up-multi
up-multi: ## Run TWO app pods (:8080 and :8081) sharing Redis, to see cross-pod sync
	docker compose --profile multi up --build

.PHONY: down
down: ## Tear down the full stack
	docker compose down

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin
