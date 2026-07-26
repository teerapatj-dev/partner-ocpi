.PHONY: build test vet run up down logs demo-up demo-down demo-logs batch-jobs

DEMO_COMPOSE = docker compose --env-file .env.demo -f docker-compose.yml -f docker-compose.demo.yml

# The demo's Roaming Out cron buttons start the real batch-ocpi-process binaries. They are built here
# rather than in the demo image because that repo pulls a private module: the host already has it in
# its module cache, a clean Docker build would need credentials. Output is gitignored and mounted
# read-only into the demo container.
BATCH_SRC ?= ../batch-ocpi-process
BATCH_OUT := deploy/batch-jobs
BATCH_ARCH ?= $(shell docker version --format '{{.Server.Arch}}' 2>/dev/null || echo arm64)

batch-jobs:
	@test -d "$(BATCH_SRC)" || { echo "batch repo not found at $(BATCH_SRC) — pass BATCH_SRC=<path>"; exit 1; }
	mkdir -p $(BATCH_OUT)/bin
	rm -rf $(BATCH_OUT)/config
	cp -R "$(BATCH_SRC)/config" $(BATCH_OUT)/config
	cp "$$(go env GOROOT)/lib/time/zoneinfo.zip" $(BATCH_OUT)/zoneinfo.zip
	cd "$(BATCH_SRC)" && CGO_ENABLED=0 GOOS=linux GOARCH=$(BATCH_ARCH) go build -trimpath \
		-o "$(CURDIR)/$(BATCH_OUT)/bin/" ./cmd/...
	@ls -1 $(BATCH_OUT)/bin

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
