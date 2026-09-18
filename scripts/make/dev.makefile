# Spin-up targets. Two profiles:
#   make up   - everything in docker, crawler included
#   make dev  - infra in docker, crawler on the host via `make run`
# They differ only in the addresses seeded into Vault, so switching profiles
# re-seeds; both are idempotent.

COMPOSE      ?= docker compose
COMPOSE_HOST := $(COMPOSE) -f docker-compose.yml -f deployments/compose.host.yml
GO           ?= go
ENV_FILE     ?= main.env
INFRA        := vault neo4j redis broker kafka-ui jaeger

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

BOOTSTRAP := ENV_FILE=$(ENV_FILE) COMPOSE="$(COMPOSE)" \
	REDIS_PASSWORD=$(REDIS_PASSWORD) \
	NEO4J_USER=$(NEO4J_USER) NEO4J_PASSWORD=$(NEO4J_PASSWORD) \
	KAFKA_USERNAME=$(KAFKA_USERNAME) KAFKA_PASSWORD=$(KAFKA_PASSWORD) \
	KAFKA_TOPIC_TASKS=$(KAFKA_TOPIC_TASKS) KAFKA_TOPIC_RUNS=$(KAFKA_TOPIC_RUNS) \
	KAFKA_TASKS_CONSUMER_GROUP=$(KAFKA_TASKS_CONSUMER_GROUP) \
	KAFKA_RUNS_CONSUMER_GROUP=$(KAFKA_RUNS_CONSUMER_GROUP) \
	CONFIG_PATH=$(CONFIG_PATH) \
	scripts/vault-bootstrap.sh

.PHONY: help up dev run crawl down reset restart logs ps env vault-bootstrap neo4j-init build test vet tidy

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
	@echo ""
	@echo "tuning:  configs/config.yml  (workers, TTLs, parser engine)"
	@echo "         make up CONFIG_PATH=configs/other.yml to use another file"
	@echo ""
	@echo "UI: kafka-ui http://localhost:8080  jaeger http://localhost:16686"
	@echo "    neo4j    http://localhost:7474  vault  http://localhost:8200"

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
	$(BOOTSTRAP) docker
	$(MAKE) neo4j-init
	$(COMPOSE) up -d --build crawler
	@echo ""
	@echo "stack is up. follow it with: make logs"

dev: env
	@$(COMPOSE) stop crawler 2>/dev/null || true
	$(COMPOSE_HOST) up -d --wait $(INFRA)
	$(BOOTSTRAP) host
	$(MAKE) neo4j-init
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

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
