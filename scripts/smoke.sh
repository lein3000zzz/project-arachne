#!/usr/bin/env bash
# End-to-end check of a running stack: crawl a few pages through the search
# service, wait until they are indexed and ranked, then search and ask, the
# answer checked claim by claim; then build a lab of the two best pages, ask
# with it ticked, upload a file to it that only the lab may find, and delete
# it.
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
    jq -e '.verification == null or all(.verification.claims[]; (.verdict // "") != "")' /tmp/arachne-smoke-answer.json >/dev/null \
      || fail "the check left claims without a verdict"
    ok "$(jq -r '"\(.model), \(.usage.output_tokens) tokens, cites \(.cited | length)/\(.sources | length)" +
      if .verification then "; checked: " + ([.verification.claims[].verdict] | group_by(.) | map("\(length) \(.[0])") | join(", ")
        | if . == "" then "no claims" else . end) + " in \(.verification.seconds | floor) s" else "" end' /tmp/arachne-smoke-answer.json)"
    ;;
  *) fail "HTTP $code: $(jq -r .error /tmp/arachne-smoke-answer.json 2>/dev/null)" ;;
esac

step "lab plan"
top=$(jq -r '.results[0].url' <<<"$res")
second=$(jq -r '.results[1].url' <<<"$res")
plan=$(api -sf -H 'Content-Type: application/json' -X POST "$BASE/api/labs/plan" \
  -d "$(jq -nc --arg r "a smoke-test lab about $QUERY: $top and $second" '{request: $r}')") || fail "planning failed"
jq -e --arg a "$top" --arg b "$second" '[.sources[].url] | index($a) and index($b)' <<<"$plan" >/dev/null \
  || fail "the plan lost a link: $(jq -c .sources <<<"$plan")"
ok "$(jq -r '"\(.name) (\(if .model == "" then "read literally" else .model end))"' <<<"$plan")"

step "lab"
lab=$(api -sf -H 'Content-Type: application/json' -X POST "$BASE/api/labs" \
  -d "$(jq -nc --arg a "$top" --arg b "$second" --arg n "smoke $(date +%s)" \
    '{name: $n, topic: "smoke test", sources: [{url: $a, scope: "page"}, {url: $b, scope: "page"}]}')") \
  || fail "the lab was refused"
lab_id=$(jq -r .id <<<"$lab")
# The lab goes even when a step fails; its pages stay in the shared base.
trap 'api -s -o /dev/null -X DELETE "$BASE/api/labs/$lab_id"; rm -f "$header"' EXIT
for lab_run in $(jq -r '.sources[].run.id' <<<"$lab"); do
  wait_for "$lab_run" '.status == "finished"' >/dev/null
done
pages=$(api -sf "$BASE/api/labs/$lab_id" | jq .pages)
[ "$pages" -eq 2 ] || fail "the crawls finished but the lab holds $pages pages, not 2"
ok "lab $lab_id holds 2 pages"

step "answer from lab"
code=$(api -s -o /tmp/arachne-smoke-lab.json -w '%{http_code}' -G "$BASE/answer" \
  --data-urlencode "q=$QUERY" --data-urlencode "labs=$lab_id" --data-urlencode verify=0)
case "$code" in
  503) echo "skip  no chat model configured" ;;
  200)
    jq -e '.scope == "labs" and all(.sources[]; .in_lab)' /tmp/arachne-smoke-lab.json >/dev/null \
      || fail "the lab held enough, yet: $(jq -c '{scope, in_lab: [.sources[].in_lab]}' /tmp/arachne-smoke-lab.json)"
    [ "$(jq '.cited | length' /tmp/arachne-smoke-lab.json)" -gt 0 ] || fail "the answer cites no source"
    ok "$(jq -r '"scope \(.scope), cites \(.cited | length)/\(.sources | length)"' /tmp/arachne-smoke-lab.json)"
    ;;
  *) fail "HTTP $code: $(jq -r .error /tmp/arachne-smoke-lab.json 2>/dev/null)" ;;
esac

# An uploaded file is its owner's: found with its lab ticked, never without.
step "upload"
word="smoke$(date +%s)quokka"
upload=$(mktemp)
printf '# Umbrella ledger\n\nThe %s ledger records every borrowed umbrella with a teal stamp and a sequence number. An umbrella that comes back is crossed out in green; one that does not is chased after a week. The ledger is kept at the front desk.\n' "$word" >"$upload"
upload_run=$(api -sf -X POST "$BASE/api/labs/$lab_id/uploads" -F "files=@$upload;filename=smoke-notes.md;type=text/markdown" \
  | jq -r '.sources[] | select(.kind == "upload") | .run.id') || fail "the upload was refused"
rm -f "$upload"
wait_for "$upload_run" '.status == "finished" and .docs_indexed + .docs_rejected >= 1' >/dev/null
found() { api -sf -G "$BASE/search" --data-urlencode "q=$word" --data-urlencode mode=bm25 --data-urlencode rerank=false "$@" \
  | jq '[.results[] | select(.url | startswith("https://uploads.arachne.invalid/"))] | length'; }
[ "$(found --data-urlencode "labs=$lab_id")" -ge 1 ] || fail "the lab does not find its own upload"
[ "$(found)" -eq 0 ] || fail "the shared search found an uploaded file"
ok "found with lab $lab_id ticked, not without"

echo "passed in $(( $(date +%s) - started )) s"
