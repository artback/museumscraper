.PHONY: help up down build test lint run-parser run-enricher run-api migrate-up migrate-down clean logs

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

up: ## Start all infrastructure services
	docker compose up -d

down: ## Stop all infrastructure services
	docker compose down

build: ## Build all Go binaries
	go build -o bin/parser ./cmd/parser
	go build -o bin/enricher ./cmd/enricher
	go build -o bin/api ./cmd/api

test: ## Run all tests
	go test -v -race -count=1 ./...

test-coverage: ## Run tests with coverage report
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

lint: ## Run linter
	golangci-lint run ./...

run-parser: ## Run the Wikipedia parser/scraper
	go run ./cmd/parser

run-enricher: ## Run the enrichment pipeline
	go run ./cmd/enricher

run-api: ## Run the query API server
	go run ./cmd/api

migrate-up: ## Run database migrations
	go run ./cmd/migrate --direction=up

migrate-down: ## Run database migrations (down)
	go run ./cmd/migrate --direction=down

clean: ## Clean build artifacts
	rm -rf bin/ coverage.out coverage.html

logs: ## Tail docker compose logs
	docker compose logs -f

db-shell: ## Open psql shell to museum database
	docker exec -it museum-postgres psql -U museum -d museumdb

wait-healthy: ## Wait for all services to be healthy
	@echo "Waiting for services..."
	@until docker compose ps --format json | grep -q '"Health":"healthy"'; do sleep 2; done
	@echo "All services ready!"
