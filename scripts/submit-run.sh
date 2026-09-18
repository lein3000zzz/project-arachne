#!/usr/bin/env bash
# Publishes a config.Run to the runs topic. There is no HTTP API, so this is how
# a crawl gets started.
#
# usage: scripts/submit-run.sh <url> [max_depth] [max_links]
set -euo pipefail

URL="${1:?usage: submit-run.sh <url> [max_depth] [max_links]}"
MAX_DEPTH="${2:-2}"
MAX_LINKS="${3:-25}"

COMPOSE="${COMPOSE:-docker compose}"
KAFKA_TOPIC_RUNS="${KAFKA_TOPIC_RUNS:-arachne.runs}"
KAFKA_USERNAME="${KAFKA_USERNAME:-arachne}"
KAFKA_PASSWORD="${KAFKA_PASSWORD:-arachne-secret}"
USE_CACHE="${USE_CACHE:-true}"

RUN_ID=$(python3 -c 'import secrets; print(secrets.token_hex(20))')

PAYLOAD=$(python3 - "$RUN_ID" "$URL" "$MAX_DEPTH" "$MAX_LINKS" "$USE_CACHE" <<'PY'
import json, sys
run_id, url, depth, links, use_cache = sys.argv[1:6]
print(json.dumps({
    "id": run_id,
    "use_cache_flag": use_cache == "true",
    "max_depth": int(depth),
    "max_links": int(links),
    "start_url": url,
}))
PY
)

printf '%s\n' "$PAYLOAD" | $COMPOSE exec -T \
  -e ARACHNE_USER="$KAFKA_USERNAME" \
  -e ARACHNE_PASS="$KAFKA_PASSWORD" \
  -e ARACHNE_TOPIC="$KAFKA_TOPIC_RUNS" \
  broker sh -c '
    set -e
    cat > /tmp/arachne-client.properties <<EOF
security.protocol=SASL_PLAINTEXT
sasl.mechanism=PLAIN
sasl.jaas.config=org.apache.kafka.common.security.plain.PlainLoginModule required username="$ARACHNE_USER" password="$ARACHNE_PASS";
EOF
    exec /opt/kafka/bin/kafka-console-producer.sh \
      --bootstrap-server broker:9092 \
      --topic "$ARACHNE_TOPIC" \
      --producer.config /tmp/arachne-client.properties
  ' >/dev/null

echo "submitted run $RUN_ID  url=$URL depth=$MAX_DEPTH links=$MAX_LINKS"
