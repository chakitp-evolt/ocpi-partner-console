.PHONY: build run test test-integration lint docker-build db-up db-down up down

APP := evolt-ocpi-partner-console

build:
	go build -o bin/$(APP) ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

# Needs an empty database named partner_console_test; the test drops and recreates its schema.
test-integration:
	go test -tags integration ./...

lint:
	go vet ./...

docker-build:
	docker build -t $(APP):local .

# Local Postgres only, starts empty (apply scripts/db by hand).
db-up:
	docker compose up -d postgres

db-down:
	docker compose down

up:
	docker compose up --build -d

down:
	docker compose down
