# goredis developer tasks. Run "make help" for the list.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
BIN_DIR   ?= bin
CMDS      := goredis goredis-check-aof goredis-bench
IMAGE     ?= goredis:dev
COMPOSE   := docker compose -f deploy/docker/docker-compose.yml
K8S_DIR   := deploy/kubernetes
FUZZTIME  ?= 30s

GOVULNCHECK_VERSION ?= v1.9.0
STATICCHECK_VERSION ?= v0.8.2

.PHONY: build test race fuzz vet fmt fmt-check lint bench cover \
        docker-build docker-run smoke compose-up compose-down \
        k8s-apply k8s-delete clean help

build: ## Build all binaries into bin/ (static, trimmed, versioned)
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "go build $$cmd"; \
		CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$$cmd ./cmd/$$cmd; \
	done

test: ## Run unit and integration tests
	$(GO) test -count=1 ./...

race: ## Run tests with the race detector (needs CGO and a C compiler)
	$(GO) test -race -count=1 ./...

fuzz: ## Fuzz the RESP parser (FUZZTIME=30s)
	$(GO) test -run='^$$' -fuzz=FuzzReadCommand -fuzztime=$(FUZZTIME) ./internal/protocol

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format all Go files in place
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "not gofmt-formatted:"; echo "$$out"; exit 1; fi

lint: fmt-check vet ## fmt-check, vet, staticcheck and govulncheck (pinned versions)
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

bench: ## Run Go benchmarks
	$(GO) test -run='^$$' -bench=. -benchmem ./...

cover: ## Run tests with coverage and print a per-function summary
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1
	@echo "HTML report: go tool cover -html=coverage.out"

docker-build: ## Build the container image (IMAGE=goredis:dev)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run: ## Run the image locally on 127.0.0.1:6379 / :9379 with a named volume
	docker run --rm --name goredis \
		-e GOREDIS_PROTECTED_MODE=false \
		-p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 \
		-v goredis-data:/data \
		$(IMAGE)

smoke: docker-build ## Build the image and run scripts/smoke.sh against it
	bash scripts/smoke.sh $(IMAGE)

compose-up: ## Start goredis + Prometheus + Grafana (deploy/docker)
	$(COMPOSE) up -d --build

compose-down: ## Stop the compose stack (volumes are kept; add -v to delete them)
	$(COMPOSE) down

k8s-apply: ## Apply the kustomization (create the goredis-auth Secret first)
	kubectl apply -k $(K8S_DIR)

k8s-delete: ## Delete the kustomization resources (the PVC and its data too)
	kubectl delete -k $(K8S_DIR)

clean: ## Remove build output and coverage files
	rm -rf $(BIN_DIR) dist coverage.out

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-14s %s\n", $$1, $$2}'
