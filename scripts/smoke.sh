#!/usr/bin/env bash
# End-to-end check of a running stack: crawl a few pages through the search
# service, wait until they are indexed and ranked, then search and ask.
#
# usage: scripts/smoke.sh   (SEARCH_URL, URL, QUERY, LINKS, TIMEOUT to override)
# Needs ARACHNE_TOKEN, an API token: make token NAME=<user>.
set -euo pipefail

BASE="${SEARCH_URL:-http://localhost:8090}"
URL="${URL:-https://go.dev/doc/}"
QUERY="${QUERY:-what is a goroutine}"
LINKS="${LINKS:-5}"
TIMEOUT="${TIMEOUT:-600}"

host=$(sed -E 's#^https?://([^/]+).*#\1#' <<<"$URL")
started=$(date +%s)

step() { printf '%-22s' "$1"; }
ok() { echo "ok    $*"; }
fail() { echo "FAIL  $*"; exit 1; }

[ -n "${ARACHNE_TOKEN:-}" ] || fail "ARACHNE_TOKEN is not set: make token NAME=<user>"

# The token goes to curl in a file only this user can read, not on its command
# line, where other processes could see it.
header=$(mktemp)
trap 'rm -f "$header"' EXIT
printf 'Authorization: Bearer %s\n' "$ARACHNE_TOKEN" >"$header"
api() { curl -H @"$header" "$@"; }

# Polls run until jq_filter is true; fails on a dead run or the deadline.
wait_for() {
  local id="$1" filter="$2" run status
  while :; do
    run=$(api -sf "$BASE/api/runs/$id") || fail "run $id is gone"
    status=$(jq -r .status <<<"$run")
    case "$status" in dropped|stalled) fail "run $status: $(jq -r .note <<<"$run")" ;; esac
    if jq -e "$filter" <<<"$run" >/dev/null; then
      echo "$run"
      return
    fi
    [ $(( $(date +%s) - started )) -lt "$TIMEOUT" ] || fail "timed out at status $status"
    sleep 3
  done
}

step "search service"
curl -sf "$BASE/healthz" >/dev/null || fail "not reachable at $BASE"
user=$(api -sf "$BASE/api/me" | jq -r .username) || fail "the API token was refused"
ok "$BASE as $user"

step "submit crawl"
run=$(api -sf -X POST "$BASE/api/runs" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg url "$URL" --argjson links "$LINKS" '{url: $url, links: $links, depth: 2, scope: "section"}')") \
  || fail "rejected"
id=$(jq -r .id <<<"$run")
ok "$URL ($LINKS pages, run ${id:0:12})"

step "crawl + index"
run=$(wait_for "$id" '.status == "finished"')
indexed=$(jq -r .docs_indexed <<<"$run")
[ "$indexed" -gt 0 ] || fail "nothing indexed: $(jq -c '{pages, docs_queued, docs_failed}' <<<"$run")"
ok "$(jq -r '"\(.pages) pages, \(.docs_indexed)/\(.docs_queued) indexed, \(.docs_failed) failed"' <<<"$run")"

step "rank"
wait_for "$id" '.ranked' >/dev/null
ok "PageRank recomputed"

step "search"
res=$(api -sfG "$BASE/search" --data-urlencode "q=$QUERY" --data-urlencode "host=$host" --data-urlencode limit=5) \
  || fail "search returned an error"
[ "$(jq '.results | length' <<<"$res")" -gt 0 ] || fail "no results for \"$QUERY\" on $host"
[ "$(jq '[.results[] | select(.pagerank)] | length' <<<"$res")" -gt 0 ] || fail "results carry no PageRank"
ok "$(jq -r '"\(.results | length) results in \(.took_ms) ms, top: \(.results[0].url)"' <<<"$res")"

step "answer"
code=$(api -s -o /tmp/arachne-smoke-answer.json -w '%{http_code}' -G "$BASE/answer" \
  --data-urlencode "q=$QUERY" --data-urlencode "host=$host")
case "$code" in
  503) echo "skip  no chat model configured" ;;
  200)
    [ -n "$(jq -r .answer /tmp/arachne-smoke-answer.json)" ] || fail "empty answer"
    [ "$(jq '.cited | length' /tmp/arachne-smoke-answer.json)" -gt 0 ] || fail "the answer cites no source"
    ok "$(jq -r '"\(.model), \(.usage.output_tokens) tokens, cites \(.cited | length)/\(.sources | length)"' /tmp/arachne-smoke-answer.json)"
    ;;
  *) fail "HTTP $code: $(jq -r .error /tmp/arachne-smoke-answer.json 2>/dev/null)" ;;
esac

echo "passed in $(( $(date +%s) - started )) s"
