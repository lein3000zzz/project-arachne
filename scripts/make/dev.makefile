# Spin-up targets. Two profiles:
#   make up   - everything in docker, crawler included
#   make dev  - infra in docker, crawler on the host via `make run`
# They differ only in the addresses seeded into Vault, so switching profiles
# re-seeds; both are idempotent.

COMPOSE      ?= docker compose
COMPOSE_HOST := $(COMPOSE) -f docker-compose.yml -f deployments/compose.host.yml
GO           ?= go
ENV_FILE     ?= main.env
INFRA        := vault neo4j redis broker jaeger milvus ollama
# Not waited on: nothing depends on the UI, and it fails its first start whenever
# it beats the broker up.
UI           := kafka-ui

REDIS_PASSWORD             ?= arachne-redis
NEO4J_USER                 ?= neo4j
NEO4J_PASSWORD             ?= testtest
NEO4J_DATABASE             ?= pages
KAFKA_USERNAME             ?= arachne
KAFKA_PASSWORD             ?= arachne-secret
KAFKA_TOPIC_TASKS          ?= arachne.tasks
KAFKA_TOPIC_RUNS           ?= arachne.runs
KAFKA_TASKS_CONSUMER_GROUP ?= arachne-tasks
KAFKA_RUNS_CONSUMER_GROUP  ?= arachne-runs
CONFIG_PATH                ?= configs/config.yml

# The config file is the single source of truth for the model name.
EMBEDDING_MODEL ?= $(shell sed -n 's/^[[:space:]]*model:[[:space:]]*//p' $(CONFIG_PATH) | head -1)
# Empty means the bundled Ollama. Set these to use an external OpenAI-compatible
# endpoint or an authenticated Milvus; credentials are only written when given.
EMBEDDING_BASE_URL ?=
EMBEDDING_API_KEY  ?=
MILVUS_TOKEN       ?=

BOOTSTRAP := ENV_FILE=$(ENV_FILE) COMPOSE="$(COMPOSE)" \
	REDIS_PASSWORD=$(REDIS_PASSWORD) \
	NEO4J_USER=$(NEO4J_USER) NEO4J_PASSWORD=$(NEO4J_PASSWORD) \
	KAFKA_USERNAME=$(KAFKA_USERNAME) KAFKA_PASSWORD=$(KAFKA_PASSWORD) \
	KAFKA_TOPIC_TASKS=$(KAFKA_TOPIC_TASKS) KAFKA_TOPIC_RUNS=$(KAFKA_TOPIC_RUNS) \
	KAFKA_TASKS_CONSUMER_GROUP=$(KAFKA_TASKS_CONSUMER_GROUP) \
	KAFKA_RUNS_CONSUMER_GROUP=$(KAFKA_RUNS_CONSUMER_GROUP) \
	CONFIG_PATH=$(CONFIG_PATH) \
	EMBEDDING_BASE_URL="$(EMBEDDING_BASE_URL)" EMBEDDING_API_KEY="$(EMBEDDING_API_KEY)" \
	MILVUS_TOKEN="$(MILVUS_TOKEN)" \
	scripts/vault-bootstrap.sh

.PHONY: help up dev run crawl down reset restart logs ps env vault-bootstrap neo4j-init models build test test-integration vet tidy

help:
	@echo "make up        full stack in docker (crawler included)"
	@echo "make dev       infra only, addressed for a host-run crawler"
	@echo "make run       go run the crawler on the host (after make dev)"
	@echo "make crawl URL=https://example.com/ [DEPTH=2] [LINKS=25]"
	@echo "make logs      follow crawler logs"
	@echo "make ps        service status"
	@echo "make down      stop containers, keep data"
	@echo "make reset     stop, delete volumes, bind mounts and $(ENV_FILE)"
	@echo ""
	@echo "make build / test / vet / tidy"
	@echo "make test-integration   tests against the running Milvus and embedder"
	@echo ""
	@echo "tuning:  configs/config.yml  (workers, TTLs, parser engine, embedding model)"
	@echo "         make up CONFIG_PATH=configs/other.yml to use another file"
	@echo ""
	@echo "UI: kafka-ui http://localhost:8080  jaeger http://localhost:16686"
	@echo "    neo4j    http://localhost:7474  vault  http://localhost:8200"
	@echo "    milvus   localhost:19530         ollama http://localhost:11434"

env: $(ENV_FILE)

$(ENV_FILE):
	@printf '%s\n' \
		'APP_ENV=dev' \
		'VAULT_ADDRESS=http://127.0.0.1:8200' \
		'VAULT_TOKEN=' \
		'VAULT_KEYS=' \
		'REDIS_PASSWORD=$(REDIS_PASSWORD)' \
		'CONFIG_PATH=$(CONFIG_PATH)' > $(ENV_FILE)
	@echo "wrote $(ENV_FILE)"

up: env
	$(COMPOSE) up -d --wait $(INFRA)
	$(COMPOSE) up -d $(UI)
	$(BOOTSTRAP) docker
	$(MAKE) neo4j-init
	$(MAKE) models
	$(COMPOSE) up -d --build crawler
	@echo ""
	@echo "stack is up. follow it with: make logs"

dev: env
	@$(COMPOSE) stop crawler 2>/dev/null || true
	$(COMPOSE_HOST) up -d --wait $(INFRA)
	$(COMPOSE_HOST) up -d $(UI)
	$(BOOTSTRAP) host
	$(MAKE) neo4j-init
	$(MAKE) models
	@echo ""
	@echo "infra is up. start the crawler with: make run"

run:
	$(GO) run ./cmd/web-crawler

crawl:
	@COMPOSE="$(COMPOSE)" KAFKA_TOPIC_RUNS=$(KAFKA_TOPIC_RUNS) \
		KAFKA_USERNAME=$(KAFKA_USERNAME) KAFKA_PASSWORD=$(KAFKA_PASSWORD) \
		scripts/submit-run.sh "$(or $(URL),https://example.com/)" $(or $(DEPTH),2) $(or $(LINKS),25)

vault-bootstrap: env
	$(BOOTSTRAP) $(or $(PROFILE),docker)

# A no-op when the model is present. Harmless when an external endpoint is used.
models:
	@$(COMPOSE) exec -T ollama ollama pull $(EMBEDDING_MODEL) >/dev/null
	@echo "embedding model '$(EMBEDDING_MODEL)' ready"

neo4j-init:
	@$(COMPOSE) exec -T neo4j cypher-shell \
		-u $(NEO4J_USER) -p $(NEO4J_PASSWORD) -d system --format plain \
		"CREATE DATABASE $(NEO4J_DATABASE) IF NOT EXISTS WAIT"
	@echo "neo4j database '$(NEO4J_DATABASE)' ready"

restart:
	$(COMPOSE) restart crawler

logs:
	$(COMPOSE) logs -f --tail=100 crawler

ps:
	$(COMPOSE) ps

down:
	$(COMPOSE) down

reset:
	$(COMPOSE) down -v --remove-orphans
	rm -rf deployments/opt output
	rm -f $(ENV_FILE)
	@echo "reset done. next: make up"

build:
	$(GO) build ./...

test:
	$(GO) test ./...

test-integration:
	MILVUS_ADDR=localhost:19530 $(GO) test -tags integration -count=1 ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
