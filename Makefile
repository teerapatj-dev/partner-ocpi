.PHONY: build test vet run up down logs demo-up demo-down demo-logs

DEMO_COMPOSE = docker compose --env-file .env.demo -f docker-compose.yml -f docker-compose.demo.yml

demo-up:
	$(DEMO_COMPOSE) up --build -d

demo-down:
	$(DEMO_COMPOSE) down

demo-logs:
	$(DEMO_COMPOSE) logs -f demo cloudflared plugsiam

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

run:
	go run ./cmd/server

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f plugsiam voltcity chargex
