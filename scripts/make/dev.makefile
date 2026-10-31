# Spin-up targets. Two profiles:
#   make up   - everything in docker, crawler included
#   make dev  - infra in docker, crawler on the host via `make run`
# They differ only in the addresses seeded into Vault, so switching profiles
# re-seeds; both are idempotent.

comma := ,
space := $(subst ,, )

COMPOSE      ?= docker compose
COMPOSE_HOST := $(COMPOSE) -f docker-compose.yml -f deployments/compose.host.yml
GO           ?= go
ENV_FILE     ?= main.env
EMBEDDERS    := ollama vllm
# Every optional profile, so down and reset stop whatever a previous run started.
ALL_PROFILES := ollama,vllm,reranker,documents,tools
# Not waited on: nothing depends on the UI, and it fails its first start whenever
# it beats the broker up.
UI           := kafka-ui

REDIS_PASSWORD             ?= arachne-redis
POSTGRES_PASSWORD          ?= arachne-postgres
export POSTGRES_PASSWORD
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

# Cores the bundled models (Ollama, the reranker) may use. They are pinned to
# them and told to start as many threads: left alone they count every host
# core, and more threads than cores makes them many times slower, not faster.
MODEL_CPUS ?= $(or $(OLLAMA_CPUS),4)
export MODEL_CPUS
export MODEL_CPUSET := 0-$(shell expr $(MODEL_CPUS) - 1)

# Reranker: the bundled llama.cpp server unless RERANK_BASE_URL points elsewhere.
RERANK_BASE_URL ?=
RERANK_API_KEY  ?=
RERANKER_SERVICE := $(if $(RERANK_BASE_URL),,reranker)

CONFIG_OVERLAY_vllm := ,configs/vllm.yml
CONFIG_PATH ?= configs/config.yml$(CONFIG_OVERLAY_$(EMBEDDER))

# $(call config_value,section,key): what the binaries will read - the last of the
# config files to set section.key wins. Top-level sections, scalar keys only.
config_value = $(shell awk -v section="$(1):" -v key="$(2):" \
	'/^[^[:space:]\#]/ { insec = ($$1 == section) } insec && $$1 == key { v = $$2 } END { gsub(/["\047]/, "", v); print v }' \
	$(subst $(comma), ,$(CONFIG_PATH)))

# Tika reads the documents labs add; it runs only when the config turns it on.
DOCUMENTS_SERVICE := $(if $(filter true,$(call config_value,documents,enabled)),tika)
DOCUMENTS_PROFILE := $(if $(DOCUMENTS_SERVICE),documents)

export COMPOSE_PROFILES := $(subst $(space),$(comma),$(strip $(EMBEDDER_SERVICE) $(RERANKER_SERVICE) $(DOCUMENTS_PROFILE)))

INFRA := vault postgres redis broker jaeger milvus searxng $(EMBEDDER_SERVICE) $(RERANKER_SERVICE) $(DOCUMENTS_SERVICE)


EMBEDDING_MODEL ?= $(call config_value,embedding,model)
export EMBEDDING_MODEL

# Chat model for answers. Without CHAT_BASE_URL it runs on the bundled Ollama,
# when there is one; otherwise answering is off until an endpoint is given.
CHAT_BASE_URL ?=
CHAT_API_KEY  ?=
CHAT_MODEL    ?= $(call config_value,chat,model)
CHAT_PROVIDER ?= $(call config_value,chat,provider)
CHAT_LOCAL    := $(if $(and $(filter ollama,$(EMBEDDER)),$(if $(CHAT_BASE_URL),,yes),$(filter openai-compatible,$(CHAT_PROVIDER))),yes)

# API token of the make targets that call the search service (make token). It
# reaches curl on stdin, never as an argument other processes could read.
ARACHNE_TOKEN ?= $(shell sed -n 's/^ARACHNE_TOKEN=//p' $(ENV_FILE) 2>/dev/null)
export ARACHNE_TOKEN
AUTH_HEADER := printf 'Authorization: Bearer %s\n' "$$ARACHNE_TOKEN"

BOOTSTRAP := ENV_FILE=$(ENV_FILE) COMPOSE="$(COMPOSE)" \
	REDIS_PASSWORD=$(REDIS_PASSWORD) \
	POSTGRES_PASSWORD=$(POSTGRES_PASSWORD) \
	KAFKA_USERNAME=$(KAFKA_USERNAME) KAFKA_PASSWORD=$(KAFKA_PASSWORD) \
	KAFKA_TOPIC_TASKS=$(KAFKA_TOPIC_TASKS) KAFKA_TOPIC_RUNS=$(KAFKA_TOPIC_RUNS) \
	KAFKA_TASKS_CONSUMER_GROUP=$(KAFKA_TASKS_CONSUMER_GROUP) \
	KAFKA_RUNS_CONSUMER_GROUP=$(KAFKA_RUNS_CONSUMER_GROUP) \
	CONFIG_PATH=$(CONFIG_PATH) EMBEDDER=$(EMBEDDER) \
	EMBEDDING_BASE_URL="$(EMBEDDING_BASE_URL)" EMBEDDING_API_KEY="$(EMBEDDING_API_KEY)" \
	MILVUS_TOKEN="$(MILVUS_TOKEN)" \
	CHAT_BASE_URL="$(CHAT_BASE_URL)" CHAT_API_KEY="$(CHAT_API_KEY)" \
	RERANK_BASE_URL="$(RERANK_BASE_URL)" RERANK_API_KEY="$(RERANK_API_KEY)" \
	scripts/vault-bootstrap.sh

.PHONY: help up dev run crawl search eval rank smoke user password token down reset restart logs ps env vault-bootstrap models stop-other-embedders build test test-integration vet tidy

help:
	@echo "make up        full stack in docker (crawler included)"
	@echo "make dev       infra only, addressed for a host-run crawler"
	@echo "make run       go run the crawler on the host (after make dev)"
	@echo "make crawl URL=https://example.com/ [DEPTH=2] [LINKS=25] [SCOPE=https://example.com/docs/]"
	@echo "make smoke     end-to-end check: crawl a few pages, index, rank, search, answer"
	@echo "make rank      recompute page and site PageRank from the link graph"
	@echo "make eval [DATASET=eval/sample.yml] [OUT=results.json]   nDCG/MRR/recall per mode"
	@echo "make search Q=\"query\" [MODE=hybrid|dense|bm25] [HOST=go.dev] [LIMIT=10]"
	@echo ""
	@echo "accounts (sign-up is closed; the search service must be up):"
	@echo "         make user NAME=<name> [ADMIN=1]   new account, password asked for"
	@echo "         make password NAME=<name>         reset a password"
	@echo "         make token NAME=<name>            API token for search/eval/smoke, saved in $(ENV_FILE)"
	@echo "make logs      follow crawler logs"
	@echo "make ps        service status"
	@echo "make down      stop containers, keep data"
	@echo "make reset     stop, delete volumes, bind mounts and $(ENV_FILE)"
	@echo ""
	@echo "make build / test / vet / tidy"
	@echo "make test-integration   tests against the running Milvus, Redis and Postgres"
	@echo ""
	@echo "tuning:  configs/config.yml  (workers, TTLs, parser engine, embedding model)"
	@echo "         make up CONFIG_PATH=configs/other.yml to use another file"
	@echo ""
	@echo "embedder (remembered in $(ENV_FILE), now: $(EMBEDDER)):"
	@echo "         make up EMBEDDER=ollama   bundled Ollama, fastest on Apple Silicon (MODEL_CPUS=4 by default)"
	@echo "         make up EMBEDDER=vllm     bundled vLLM (CPU), for x86 servers"
	@echo "         make up EMBEDDING_BASE_URL=https://... [EMBEDDING_API_KEY=...]   external"
	@echo ""
	@echo "chat model (answers): configs/config.yml chat.*, endpoint and key in Vault"
	@echo "         make up                   local $(CHAT_MODEL) on the bundled Ollama"
	@echo "         make up CONFIG_PATH=configs/config.yml,configs/chat-anthropic.yml \\"
	@echo "                 CHAT_BASE_URL=https://api.anthropic.com/v1 CHAT_API_KEY=..."
	@echo ""
	@echo "reranker: bundled llama.cpp (bge-reranker-v2-m3), or RERANK_BASE_URL=... [RERANK_API_KEY=...]"
	@echo ""
	@echo "UI: kafka-ui http://localhost:8080  jaeger http://localhost:16686"
	@echo "    postgres localhost:5432          vault  http://localhost:8200"
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
	$(MAKE) models
	$(COMPOSE) up -d --build crawler search ranker
	@echo ""
	@echo "stack is up. follow it with: make logs; search at http://localhost:8090/search?q=..."

dev: env
	@$(COMPOSE) stop crawler 2>/dev/null || true
	@$(MAKE) --no-print-directory stop-other-embedders
	$(COMPOSE_HOST) up -d --wait $(INFRA)
	$(COMPOSE_HOST) up -d $(UI)
	$(BOOTSTRAP) host
	$(MAKE) models
	@echo ""
	@echo "infra is up. start the crawler with: make run"

run:
	$(GO) run ./cmd/web-crawler

# make search Q="goroutine leak" [MODE=hybrid|dense|bm25] [HOST=go.dev] [LIMIT=10]
search:
	@$(AUTH_HEADER) | curl -sfG -H @- http://localhost:8090/search --data-urlencode "q=$(Q)" \
		$(if $(MODE),--data-urlencode "mode=$(MODE)") $(if $(HOST),--data-urlencode "host=$(HOST)") \
		$(if $(LIMIT),--data-urlencode "limit=$(LIMIT)") | jq .

# End to end on the running stack: crawl, index, rank, search, answer.
smoke:
	@scripts/smoke.sh

# PageRank now, without waiting for the ranker service to notice a finished crawl.
rank:
	$(COMPOSE) --profile tools run --rm --build rank

# make eval [DATASET=eval/sample.yml] [OUT=eval/results.json]; needs the search service up
eval:
	$(GO) run ./cmd/eval -dataset $(or $(DATASET),eval/sample.yml) $(if $(OUT),-out $(OUT))

# make user NAME=lein [ADMIN=1]: the password is asked for, never passed in.
user:
	@test -n "$(NAME)" || { echo "usage: make user NAME=<username> [ADMIN=1]"; exit 2; }
	@$(COMPOSE) exec search ./user create $(if $(ADMIN),-admin) "$(NAME)"

password:
	@test -n "$(NAME)" || { echo "usage: make password NAME=<username>"; exit 2; }
	@$(COMPOSE) exec search ./user password "$(NAME)"

# Replaces the account's earlier "make" token, so running it again rotates it.
token:
	@test -n "$(NAME)" || { echo "usage: make token NAME=<username>"; exit 2; }
	@token=$$($(COMPOSE) exec -T search ./user token -name make "$(NAME)") || exit 1; \
	sed -i.bak '/^ARACHNE_TOKEN=/d' $(ENV_FILE) && rm -f $(ENV_FILE).bak && \
	{ [ -z "$$(tail -c1 $(ENV_FILE))" ] || echo >> $(ENV_FILE); } && \
	echo "ARACHNE_TOKEN=$$token" >> $(ENV_FILE) && echo "saved in $(ENV_FILE)"

crawl:
	@COMPOSE="$(COMPOSE)" KAFKA_TOPIC_RUNS=$(KAFKA_TOPIC_RUNS) \
		KAFKA_USERNAME=$(KAFKA_USERNAME) KAFKA_PASSWORD=$(KAFKA_PASSWORD) \
		scripts/submit-run.sh "$(or $(URL),https://example.com/)" $(or $(DEPTH),2) $(or $(LINKS),25) "$(SCOPE)"

vault-bootstrap: env
	$(BOOTSTRAP) $(or $(PROFILE),docker)

# Ollama pulls on demand; a no-op when the model is present. vLLM downloads the
# model itself on its first start, into deployments/opt/huggingface.
# A pull resets the model's parameters, so num_thread is set again after it, and
# the model unloaded so the next request loads it with the new value.
models:
ifeq ($(EMBEDDER),ollama)
	@$(COMPOSE) exec -T ollama ollama pull $(EMBEDDING_MODEL) >/dev/null
	@$(COMPOSE) exec -T ollama sh -c 'printf "FROM %s\nPARAMETER num_thread %s\n" "$(EMBEDDING_MODEL)" "$(MODEL_CPUS)" > /tmp/Modelfile && ollama create "$(EMBEDDING_MODEL)" -f /tmp/Modelfile >/dev/null && ollama stop "$(EMBEDDING_MODEL)" 2>/dev/null || true'
endif
ifeq ($(CHAT_LOCAL),yes)
	@$(COMPOSE) exec -T ollama ollama pull $(CHAT_MODEL) >/dev/null
	@# Ollama's default 4k context would silently cut off the sources.
	@$(COMPOSE) exec -T ollama sh -c 'printf "FROM %s\nPARAMETER num_thread %s\nPARAMETER num_ctx 8192\n" "$(CHAT_MODEL)" "$(MODEL_CPUS)" > /tmp/Modelfile && ollama create "$(CHAT_MODEL)" -f /tmp/Modelfile >/dev/null && ollama stop "$(CHAT_MODEL)" 2>/dev/null || true'
	@echo "chat model '$(CHAT_MODEL)' ready (ollama on $(MODEL_CPUS) cores)"
endif
	@echo "embedding model '$(EMBEDDING_MODEL)' ready ($(EMBEDDER)$(if $(filter ollama,$(EMBEDDER)), on $(MODEL_CPUS) cores))"

stop-other-embedders:
	@COMPOSE_PROFILES=$(subst $(space),$(comma),$(EMBEDDERS)) \
		$(COMPOSE) stop $(filter-out $(EMBEDDER_SERVICE),$(EMBEDDERS)) 2>/dev/null || true

restart:
	$(COMPOSE) restart crawler

logs:
	$(COMPOSE) logs -f --tail=100 crawler

ps:
	$(COMPOSE) ps

# Every profile, so an embedder left over from a switch is stopped too.
down:
	COMPOSE_PROFILES=$(ALL_PROFILES) $(COMPOSE) down

reset:
	COMPOSE_PROFILES=$(ALL_PROFILES) $(COMPOSE) down -v --remove-orphans
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
		POSTGRES_DSN="postgres://arachne:$(POSTGRES_PASSWORD)@localhost:5432/arachne?sslmode=disable" \
		$(GO) test -tags integration -count=1 ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
