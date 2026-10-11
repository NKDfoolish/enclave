# Enclave developer commands. Run `make help` to list them.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Load local settings (ports, DATABASE_URL) if present; .env.example documents them.
-include .env
export

DATABASE_URL ?= postgres://enclave:enclave@localhost:5432/enclave?sslmode=disable
# The binary defaults to false; local `make run-api` turns debug routes on.
API_ENABLE_DEBUG_ROUTES ?= true

# Some machines only have the standalone docker-compose binary.
COMPOSE := $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo "docker-compose")

GOOSE := go run github.com/pressly/goose/v3/cmd/goose@v3.26.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
MIGRATIONS_DIR := api/migrations

.PHONY: help
help: ## Show available commands
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

##@ Infrastructure
.PHONY: up down ps logs
up: ## Start Postgres, Redis and Qdrant and wait until healthy
	$(COMPOSE) up -d --wait

down: ## Stop local infrastructure (data volumes are kept)
	$(COMPOSE) down

ps: ## Show infrastructure status
	$(COMPOSE) ps

logs: ## Follow infrastructure logs
	$(COMPOSE) logs -f

##@ Database
.PHONY: migrate-up migrate-down migrate-status migrate-new
migrate-up: ## Apply all pending migrations
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: ## Roll back the latest migration
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

migrate-status: ## Show migration status
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" status

migrate-new: ## Create a migration: make migrate-new name=create_workspaces
	$(GOOSE) -dir $(MIGRATIONS_DIR) -s create $(name) sql

##@ Code
.PHONY: proto setup lint fmt test smoke run-api run-rag
proto: ## Lint protos and regenerate Go and Python code
	buf lint
	buf generate

setup: ## Install Python dependencies
	cd rag && uv sync

lint: ## Lint Go, Python and protos
	buf lint
	$(GOLANGCI_LINT) run ./...
	cd rag && uv run ruff check . && uv run ruff format --check .

fmt: ## Format Go and Python code
	gofmt -w api gateway mcp
	cd rag && uv run ruff check --fix . && uv run ruff format .

test: ## Run Go and Python tests
	go test -race ./...
	cd rag && uv run pytest

smoke: ## End-to-end check: Go API calls Python RAG over gRPC
	./scripts/smoke.sh

run-rag: ## Run the RAG gRPC server
	cd rag && PYTHONPATH=src:gen uv run python -m rag.main

run-api: ## Run the Core API
	go run ./api/cmd/api
