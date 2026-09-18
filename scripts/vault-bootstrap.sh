#!/usr/bin/env bash
# Initialises, unseals and seeds Vault, then records the root token and unseal
# key in the env file. Idempotent: safe to re-run, and re-running with a
# different profile just re-points the seeded addresses.
#
# usage: scripts/vault-bootstrap.sh <docker|host>
set -euo pipefail

PROFILE="${1:-docker}"
ENV_FILE="${ENV_FILE:-main.env}"
COMPOSE="${COMPOSE:-docker compose}"

REDIS_PASSWORD="${REDIS_PASSWORD:-arachne-redis}"
NEO4J_USER="${NEO4J_USER:-neo4j}"
NEO4J_PASSWORD="${NEO4J_PASSWORD:-testtest}"
KAFKA_USERNAME="${KAFKA_USERNAME:-arachne}"
KAFKA_PASSWORD="${KAFKA_PASSWORD:-arachne-secret}"
KAFKA_TOPIC_TASKS="${KAFKA_TOPIC_TASKS:-arachne.tasks}"
KAFKA_TOPIC_RUNS="${KAFKA_TOPIC_RUNS:-arachne.runs}"
KAFKA_TASKS_CONSUMER_GROUP="${KAFKA_TASKS_CONSUMER_GROUP:-arachne-tasks}"
KAFKA_RUNS_CONSUMER_GROUP="${KAFKA_RUNS_CONSUMER_GROUP:-arachne-runs}"
CONFIG_PATH="${CONFIG_PATH:-configs/config.yml}"

case "$PROFILE" in
  docker)
    APP_ENV=prod
    VAULT_ADDRESS=http://vault:8200
    REDIS_URI=redis:6379
    NEO4J_URI=bolt://neo4j:7687
    KAFKA_ADDR=broker:9092
    OTLP_ENDPOINT=jaeger:4318
    ;;
  host)
    APP_ENV=dev
    VAULT_ADDRESS=http://127.0.0.1:8200
    REDIS_URI=localhost:6379
    NEO4J_URI=bolt://localhost:7687
    KAFKA_ADDR=localhost:29092
    OTLP_ENDPOINT=localhost:4318
    ;;
  *)
    echo "unknown profile '$PROFILE' (want: docker | host)" >&2
    exit 1
    ;;
esac

set_env() {
  python3 - "$ENV_FILE" "$1" "$2" <<'PY'
import io, sys
path, key, value = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    lines = io.open(path, encoding="utf-8").read().splitlines()
except FileNotFoundError:
    lines = []
prefix, replaced, out = key + "=", False, []
for line in lines:
    if line.startswith(prefix):
        out.append(prefix + value)
        replaced = True
    else:
        out.append(line)
if not replaced:
    out.append(prefix + value)
io.open(path, "w", encoding="utf-8").write("\n".join(out) + "\n")
PY
}

get_env() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -1
}

vault_anon() {
  $COMPOSE exec -T -e VAULT_ADDR=http://127.0.0.1:8200 vault vault "$@"
}

vault_auth() {
  $COMPOSE exec -T -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN="$ROOT_TOKEN" vault vault "$@"
}

echo "==> waiting for vault"
status=""
for _ in $(seq 1 60); do
  rc=0
  status=$(vault_anon status -format=json 2>/dev/null) || rc=$?
  # 0 = unsealed, 2 = sealed; both mean the API is answering.
  if [ "$rc" -eq 0 ] || { [ "$rc" -eq 2 ] && [ -n "$status" ]; }; then
    break
  fi
  status=""
  sleep 1
done

if [ -z "$status" ]; then
  echo "vault did not become reachable" >&2
  exit 1
fi

initialized=$(jq -r '.initialized' <<<"$status")
sealed=$(jq -r '.sealed' <<<"$status")

ROOT_TOKEN=$(get_env VAULT_TOKEN)
UNSEAL_KEY=$(get_env VAULT_KEYS)

if [ "$initialized" != "true" ]; then
  echo "==> initialising vault"
  init_json=$(vault_anon operator init -key-shares=1 -key-threshold=1 -format=json)
  ROOT_TOKEN=$(jq -r '.root_token' <<<"$init_json")
  UNSEAL_KEY=$(jq -r '.unseal_keys_b64[0]' <<<"$init_json")
  set_env VAULT_TOKEN "$ROOT_TOKEN"
  set_env VAULT_KEYS "$UNSEAL_KEY"
  echo "    root token and unseal key written to $ENV_FILE"
  sealed=true
fi

if [ -z "$ROOT_TOKEN" ] || [ -z "$UNSEAL_KEY" ]; then
  echo "vault is initialised but $ENV_FILE has no VAULT_TOKEN/VAULT_KEYS." >&2
  echo "the root token cannot be recovered - run 'make reset' to start clean." >&2
  exit 1
fi

if [ "$sealed" = "true" ]; then
  echo "==> unsealing vault"
  vault_anon operator unseal "$UNSEAL_KEY" >/dev/null
fi

if ! vault_auth secrets list -format=json | jq -e '."kv/"' >/dev/null 2>&1; then
  echo "==> enabling kv-v2 at kv/"
  vault_auth secrets enable -path=kv kv-v2 >/dev/null
fi

# vault-config-manager LISTs kv/metadata/main/ and reads each child, so every
# secret has to live in a subfolder of main/ - keys written at kv/main itself
# are never picked up.
echo "==> seeding kv/main (profile: $PROFILE)"
vault_auth kv put kv/main/redis \
  REDIS_URI="$REDIS_URI" \
  REDIS_PASSWORD="$REDIS_PASSWORD" >/dev/null
vault_auth kv put kv/main/neo4j \
  NEO4J_URI="$NEO4J_URI" \
  NEO4J_USER="$NEO4J_USER" \
  NEO4J_PASSWORD="$NEO4J_PASSWORD" >/dev/null
vault_auth kv put kv/main/kafka \
  KAFKA_ADDR="$KAFKA_ADDR" \
  KAFKA_USERNAME="$KAFKA_USERNAME" \
  KAFKA_PASSWORD="$KAFKA_PASSWORD" \
  KAFKA_TOPIC_TASKS="$KAFKA_TOPIC_TASKS" \
  KAFKA_TOPIC_RUNS="$KAFKA_TOPIC_RUNS" \
  KAFKA_TASKS_CONSUMER_GROUP="$KAFKA_TASKS_CONSUMER_GROUP" \
  KAFKA_RUNS_CONSUMER_GROUP="$KAFKA_RUNS_CONSUMER_GROUP" >/dev/null
vault_auth kv put kv/main/otel \
  OTLP_ENDPOINT="$OTLP_ENDPOINT" >/dev/null

set_env APP_ENV "$APP_ENV"
set_env VAULT_ADDRESS "$VAULT_ADDRESS"
set_env REDIS_PASSWORD "$REDIS_PASSWORD"
set_env CONFIG_PATH "$CONFIG_PATH"

echo "==> vault ready"
