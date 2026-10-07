# Spin-up targets. Two profiles:
#   make up   - everything in docker, crawler included
#   make dev  - infra in docker, crawler on the host via `make run`
# They differ only in the addresses seeded into Vault, so switching profiles
# re-seeds; both are idempotent.

COMPOSE      ?= docker compose
COMPOSE_HOST := $(COMPOSE) -f docker-compose.yml -f deployments/compose.host.yml
GO           ?= go
ENV_FILE     ?= main.env
EMBEDDERS    := ollama vllm
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

# Embedding backend: ollama (fastest on Apple Silicon), vllm (x86 servers), or
# external (EMBEDDING_BASE_URL). Giving EMBEDDING_BASE_URL alone means external.
# The choice is remembered in $(ENV_FILE), so a plain `make up` keeps it: a silent
# switch would move indexing to another model's collection.
EMBEDDING_BASE_URL ?=
EMBEDDING_API_KEY  ?=
MILVUS_TOKEN       ?=
EMBEDDER ?= $(if $(EMBEDDING_BASE_URL),external,$(or $(shell sed -n 's/^EMBEDDER=//p' $(ENV_FILE) 2>/dev/null),ollama))
EMBEDDER_SERVICE := $(filter $(EMBEDDER),$(EMBEDDERS))
export COMPOSE_PROFILES := $(EMBEDDER_SERVICE)

INFRA := vault neo4j redis broker jaeger milvus $(EMBEDDER_SERVICE)

comma := ,
space := $(subst ,, )
CONFIG_OVERLAY_vllm := ,configs/vllm.yml
CONFIG_PATH ?= configs/config.yml$(CONFIG_OVERLAY_$(EMBEDDER))

# The config files are the single source of truth for the model name; the last
# file naming one wins, as in the crawler.
EMBEDDING_MODEL ?= $(shell sed -n 's/^[[:space:]]*model:[[:space:]]*//p' $(subst $(comma), ,$(CONFIG_PATH)) | tail -1)
export EMBEDDING_MODEL

BOOTSTRAP := ENV_FILE=$(ENV_FILE) COMPOSE="$(COMPOSE)" \
	REDIS_PASSWORD=$(REDIS_PASSWORD) \
	NEO4J_USER=$(NEO4J_USER) NEO4J_PASSWORD=$(NEO4J_PASSWORD) \
	KAFKA_USERNAME=$(KAFKA_USERNAME) KAFKA_PASSWORD=$(KAFKA_PASSWORD) \
	KAFKA_TOPIC_TASKS=$(KAFKA_TOPIC_TASKS) KAFKA_TOPIC_RUNS=$(KAFKA_TOPIC_RUNS) \
	KAFKA_TASKS_CONSUMER_GROUP=$(KAFKA_TASKS_CONSUMER_GROUP) \
	KAFKA_RUNS_CONSUMER_GROUP=$(KAFKA_RUNS_CONSUMER_GROUP) \
	CONFIG_PATH=$(CONFIG_PATH) EMBEDDER=$(EMBEDDER) \
	EMBEDDING_BASE_URL="$(EMBEDDING_BASE_URL)" EMBEDDING_API_KEY="$(EMBEDDING_API_KEY)" \
	MILVUS_TOKEN="$(MILVUS_TOKEN)" \
	scripts/vault-bootstrap.sh

.PHONY: help up dev run crawl down reset restart logs ps env vault-bootstrap neo4j-init models stop-other-embedders build test test-integration vet tidy

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
	@echo "make test-integration   tests against the running Milvus, Redis and Neo4j"
	@echo ""
	@echo "tuning:  configs/config.yml  (workers, TTLs, parser engine, embedding model)"
	@echo "         make up CONFIG_PATH=configs/other.yml to use another file"
	@echo ""
	@echo "embedder (remembered in $(ENV_FILE), now: $(EMBEDDER)):"
	@echo "         make up EMBEDDER=ollama   bundled Ollama, fastest on Apple Silicon"
	@echo "         make up EMBEDDER=vllm     bundled vLLM (CPU), for x86 servers"
	@echo "         make up EMBEDDING_BASE_URL=https://... [EMBEDDING_API_KEY=...]   external"
	@echo ""
	@echo "UI: kafka-ui http://localhost:8080  jaeger http://localhost:16686"
	@echo "    neo4j    http://localhost:7474  vault  http://localhost:8200"
	@echo "    milvus   localhost:19530         ollama http://localhost:11434  vllm http://localhost:8001"

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
	@$(MAKE) --no-print-directory stop-other-embedders
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
	@$(MAKE) --no-print-directory stop-other-embedders
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

# Ollama pulls on demand; a no-op when the model is present. vLLM downloads the
# model itself on its first start, into deployments/opt/huggingface.
models:
ifeq ($(EMBEDDER),ollama)
	@$(COMPOSE) exec -T ollama ollama pull $(EMBEDDING_MODEL) >/dev/null
endif
	@echo "embedding model '$(EMBEDDING_MODEL)' ready ($(EMBEDDER))"

stop-other-embedders:
	@COMPOSE_PROFILES=$(subst $(space),$(comma),$(EMBEDDERS)) \
		$(COMPOSE) stop $(filter-out $(EMBEDDER_SERVICE),$(EMBEDDERS)) 2>/dev/null || true

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

# Every profile, so an embedder left over from a switch is stopped too.
down:
	COMPOSE_PROFILES=$(subst $(space),$(comma),$(EMBEDDERS)) $(COMPOSE) down

reset:
	COMPOSE_PROFILES=$(subst $(space),$(comma),$(EMBEDDERS)) $(COMPOSE) down -v --remove-orphans
	rm -rf deployments/opt output
	rm -f $(ENV_FILE)
	@echo "reset done. next: make up"

build:
	$(GO) build ./...

test:
	$(GO) test ./...

test-integration:
	MILVUS_ADDR=localhost:19530 REDIS_ADDR=localhost:6379 \
		REDIS_PASSWORD="$$(sed -n 's/^REDIS_PASSWORD=//p' $(ENV_FILE))" \
		NEO4J_URI=bolt://localhost:7687 \
		NEO4J_PASSWORD="$$(sed -n 's/^NEO4J_AUTH="*[^/]*\/\([^"]*\)"*$$/\1/p' deployments/neo4j/neo4j.env)" \
		$(GO) test -tags integration -count=1 ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
