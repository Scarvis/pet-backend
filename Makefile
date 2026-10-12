# FinanceApp backend — build, test, lint, run and migrations.
#
# Migrations use golang-migrate (https://github.com/golang-migrate/migrate). The
# DATABASE_URL is read from the environment or from a .env file; export it before
# running migrate-* targets:
#   export DATABASE_URL=postgres://app:<password>@localhost:5432/financeapp?sslmode=disable

MIGRATIONS_DIR := migrations

ifeq ($(origin DATABASE_URL), environment)
DBURL := $(DATABASE_URL)
else ifneq (,$(shell grep -E '^DATABASE_URL=' .env 2>/dev/null))
DBURL := $(shell grep -E '^DATABASE_URL=' .env | head -n1 | cut -d= -f2-)
else
$(warning DATABASE_URL is not set. Export it or add it to .env before running migrate-* targets.)
endif

.PHONY: help build test lint run migrate-up migrate-down migrate-create

help:
	@echo "Targets:"
	@echo "  make build       go build ./..."
	@echo "  make test        go test -race -cover ./..."
	@echo "  make lint        golangci-lint run"
	@echo "  make run         go run ./cmd/server"
	@echo "  make migrate-up  apply pending migrations (needs DATABASE_URL)"
	@echo "  make migrate-down roll back one migration step"
	@echo "  make migrate-create <name> create and apply a new migration"

build:
	go build ./...

test:
	go test -race -cover ./...

lint:
	golangci-lint run

run:
	go run ./cmd/server

migrate-up:
	migrate -path $(MIGRATIONS_DIR) -database "$(DBURL)" up

migrate-down:
	migrate -path $(MIGRATIONS_DIR) -database "$(DBURL)" down

migrate-create:
	@echo "Usage: make migrate-create name=<migration-name>"
	-migrate create -msg "$$(name)" -path $(MIGRATIONS_DIR) -database "$(DBURL)" up
