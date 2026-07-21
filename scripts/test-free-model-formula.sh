#!/bin/sh
set -eu

root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
normalize="$root/scripts/normalize-free-model-research.sh"
build="$root/scripts/build-free-model-manifest.sh"
normalize_candidates="$root/scripts/normalize-provider-candidates.sh"
apply_probed="$root/scripts/apply-probed-inventory.sh"
filter_providers="$root/scripts/filter-free-model-providers.sh"

providers='[
  {"id":"groq","register_url":"https://console.groq.com/keys"},
  {"id":"gemini","register_url":"https://aistudio.google.com/apikey"},
  {"id":"pollinations","register_url":"https://enter.pollinations.ai/"}
]'
research='[
  {"provider":"pollinations","policy":"all-listed","free_basis":"Claim without direct evidence.","source_urls":["https://enter.pollinations.ai/"],"models":[]},
  {"provider":"groq","policy":"all-listed","free_basis":"Free Plan quota.","source_urls":["https://console.groq.com/docs/rate-limits"],"models":[]},
  "{\"provider\":\"groq\",\"policy\":\"all-listed\",\"free_basis\":\"wrong iteration\",\"source_urls\":[\"https://console.groq.com/docs/rate-limits\"],\"models\":[]}{\"provider\":\"gemini\"}"
]'

selected="$(sh "$filter_providers" "$providers" gemini)"
test "$(printf '%s' "$selected" | jq 'length')" -eq 1
test "$(printf '%s' "$selected" | jq -r '.[0].id')" = "gemini"
test "$(sh "$filter_providers" "$providers" all | jq 'length')" -eq 3
if sh "$filter_providers" "$providers" missing >/dev/null 2>&1; then
  echo "unknown provider unexpectedly passed filtering" >&2
  exit 1
fi

normalized="$(sh "$normalize" "$providers" "$research")"
test "$(printf '%s' "$normalized" | jq 'length')" -eq 3
test "$(printf '%s' "$normalized" | jq '[.[] | select(.accepted == true)] | length')" -eq 1
test "$(printf '%s' "$normalized" | jq -r '.[] | select(.accepted == true) | .provider')" = "groq"
test "$(printf '%s' "$normalized" | jq -r '.[] | select(.provider == "gemini") | .validation_error')" = "no result was returned for the provider"

current='{
  "schema_version":1,
  "generated_at":"2026-07-20T00:00:00Z",
  "providers":{
    "groq":{"policy":"all-listed","free_basis":"Free Plan quota.","source_urls":["https://console.groq.com/docs/rate-limits"]},
    "gemini":{"policy":"inventory","free_basis":"Verified inventory.","source_urls":["https://ai.google.dev/models"],"models":[{"id":"gemini-free","functions":["chat"],"verified_at":"2026-07-20T00:00:00Z"}]}
  }
}'
unchanged="$(sh "$build" "$current" "$normalized" "2026-07-21T00:00:00Z")"
test "$(printf '%s' "$unchanged" | jq -r '.generated_at')" = "2026-07-20T00:00:00Z"
test "$(printf '%s' "$unchanged" | jq -r '.providers.gemini.models[0].id')" = "gemini-free"

inventory='[{"provider":"gemini","policy":"inventory","free_basis":"Updated official inventory.","source_urls":["https://ai.google.dev/models"],"models":[{"id":"gemini-new","functions":["chat","chat-tools"],"source_urls":["https://ai.google.dev/models"]}],"accepted":true}]'
updated="$(sh "$build" "$current" "$inventory" "2026-07-21T00:00:00Z")"
test "$(printf '%s' "$updated" | jq -r '.generated_at')" = "2026-07-21T00:00:00Z"
test "$(printf '%s' "$updated" | jq -r '.providers.gemini.models[0].id')" = "gemini-new"
test "$(printf '%s' "$updated" | jq -r '.providers.gemini.models[0].verified_at')" = "2026-07-21T00:00:00Z"

discovery='{"candidates":[{"id":"groq","base_url":"https://api.groq.com/openai/v1","register_url":"https://console.groq.com/keys","api_key_env":"GROQ_API_KEY","free_basis":"duplicate","source_urls":["https://console.groq.com/docs/rate-limits"],"models":[]},{"id":"new-free","base_url":"https://api.example.com/v1","register_url":"https://example.com/register","api_key_env":"NEW_FREE_API_KEY","free_basis":"Documented free quota.","source_urls":["https://example.com/docs/free-tier"],"models":[]}],"notes":"fixture"}'
candidates="$(sh "$normalize_candidates" "$providers" "$discovery")"
test "$(printf '%s' "$candidates" | jq -r '.candidates[0].id')" = "new-free"
test "$(printf '%s' "$candidates" | jq -r '.rejected_candidate_ids[0]')" = "groq"

probed='{"providers":[{"provider":"groq","models":[{"id":"verified-chat","functions":["chat"]}]}],"fetch_failures":[],"probe_failures":[]}'
admitted="$(sh "$apply_probed" "$current" "$probed" "2026-07-22T00:00:00Z" all)"
test "$(printf '%s' "$admitted" | jq -r '.providers.groq.policy')" = "inventory"
test "$(printf '%s' "$admitted" | jq -r '.providers.groq.models[0].id')" = "verified-chat"
test "$(printf '%s' "$admitted" | jq -r '.providers.groq.models[0].verified_at')" = "2026-07-22T00:00:00Z"
test "$(printf '%s' "$admitted" | jq -r '.providers.gemini.policy')" = "unverified"
test "$(printf '%s' "$admitted" | jq '.providers.gemini.models | length')" -eq 0

targeted="$(sh "$apply_probed" "$current" '{"providers":[{"provider":"gemini","models":[{"id":"gemini-targeted","functions":["chat"]}]}]}' "2026-07-23T00:00:00Z" gemini)"
test "$(printf '%s' "$targeted" | jq -r '.providers.gemini.models[0].id')" = "gemini-targeted"
test "$(printf '%s' "$targeted" | jq -r '.providers.groq.policy')" = "all-listed"
test "$(printf '%s' "$targeted" | jq '.providers.groq | has("models")')" = "false"

printf 'free-model formula fixture tests passed\n'
