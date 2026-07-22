#!/bin/sh
set -eu

root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
normalize="$root/scripts/normalize-free-model-research.sh"
build="$root/scripts/build-free-model-manifest.sh"
normalize_candidates="$root/scripts/normalize-provider-candidates.sh"
collect_research="$root/scripts/collect-free-model-research.sh"
filter_providers="$root/scripts/filter-free-model-providers.sh"
resolve_inventory="$root/scripts/resolve-free-model-inventory.sh"
quality_gate="$root/scripts/check-free-model-manifest-quality.sh"
summarize="$root/scripts/summarize-free-model-run.sh"
valid_candidate='{"valid":true,"error":"","provider_count":2,"model_count":2}'
candidate_dir="$(mktemp -d)"
trap 'rm -rf "$candidate_dir"' EXIT HUP INT TERM

providers='[
  {"id":"groq","model_discovery":"api","register_url":"https://console.groq.com/keys"},
  {"id":"gemini","model_discovery":"api-agent-filter","register_url":"https://aistudio.google.com/apikey","source_urls":["https://ai.google.dev/gemini-api/docs/rate-limits"]},
  {"id":"pollinations","model_discovery":"agent","register_url":"https://enter.pollinations.ai/","source_urls":["https://enter.pollinations.ai/"]}
]'
research='[
  {"provider":"pollinations","free_basis":"Claim without direct evidence.","source_urls":["https://enter.pollinations.ai/"],"models":[]},
  {"provider":"groq","free_basis":"Free Plan quota.","source_urls":["https://console.groq.com/docs/rate-limits"],"models":[{"id":"groq-free","functions":["chat"]}]},
  {"provider":"gemini","free_basis":"Free tier.","source_urls":["https://ai.google.dev/gemini-api/docs/pricing"],"models":[{"id":"models/gemini-test","functions":["chat"]}]}
]'

selected="$(sh "$filter_providers" "$providers" gemini)"
test "$(printf '%s' "$selected" | jq '.providers | length')" -eq 1
test "$(printf '%s' "$selected" | jq -r '.providers[0].id')" = "gemini"
test "$(printf '%s' "$selected" | jq -r '.gemini')" = "true"
test "$(printf '%s' "$selected" | jq -r '.research_gemini')" = "true"
test "$(printf '%s' "$selected" | jq -r '.all')" = "false"
all_selected="$(sh "$filter_providers" "$providers" all)"
test "$(printf '%s' "$all_selected" | jq '.providers | length')" -eq 3
test "$(printf '%s' "$all_selected" | jq -r '.all')" = "true"
test "$(printf '%s' "$all_selected" | jq -r '.groq and .gemini and .pollinations')" = "true"
test "$(printf '%s' "$all_selected" | jq -r '(.research_groq|not) and .research_gemini and .research_pollinations')" = "true"
if sh "$filter_providers" "$providers" missing >/dev/null 2>&1; then
  echo "unknown provider unexpectedly passed filtering" >&2
  exit 1
fi

collected="$(sh "$collect_research" '"{\"provider\":\"groq\",\"models\":[]}"' '{{research-gemini}}')"
test "$(printf '%s' "$collected" | jq 'length')" -eq 1
test "$(printf '%s' "$collected" | jq -r '.[0].provider')" = "groq"

normalized="$(sh "$normalize" "$providers" "$research")"
test "$(printf '%s' "$normalized" | jq 'length')" -eq 2
test "$(printf '%s' "$normalized" | jq '[.[] | select(.accepted == true)] | length')" -eq 1
test "$(printf '%s' "$normalized" | jq -r '.[] | select(.accepted == true) | .provider')" = "gemini"
test "$(printf '%s' "$normalized" | jq -r '.[] | select(.provider == "pollinations") | .accepted')" = "false"

third_party='[{"provider":"gemini","free_basis":"Untrusted summary.","source_urls":["https://example.com/free"],"models":[{"id":"models/gemini-test","functions":["chat"],"source_urls":["https://example.com/model"]}]}]'
third_party_normalized="$(sh "$normalize" "$selected" "$third_party")"
test "$(printf '%s' "$third_party_normalized" | jq -r '.[0].accepted')" = "false"
test "$(printf '%s' "$third_party_normalized" | jq -r '.[0].validation_error')" = "provider result did not match the required schema or evidence rules"

selected_normalized="$(sh "$normalize" "$selected" '[{"provider":"gemini","free_basis":"Free tier.","source_urls":["https://ai.google.dev/gemini-api/docs/pricing"],"models":[{"id":"models/gemini-test","functions":["chat"]}]}]')"
test "$(printf '%s' "$selected_normalized" | jq 'length')" -eq 1
test "$(printf '%s' "$selected_normalized" | jq -r '.[0].provider')" = "gemini"

current='{
  "schema_version":2,
  "generated_at":"2026-07-20T00:00:00Z",
  "providers":{
    "groq":{"free_basis":"Free Plan quota.","source_urls":["https://console.groq.com/docs/rate-limits"],"models":[{"id":"verified-chat","functions":["chat"],"verified_at":"2026-07-20T00:00:00Z"}]},
    "gemini":{"free_basis":"Verified inventory.","source_urls":["https://ai.google.dev/models"],"models":[{"id":"gemini-free","functions":["chat"],"verified_at":"2026-07-20T00:00:00Z"}]}
  }
}'
official='{"providers":[{"provider":"groq","models":[{"id":"groq-new","functions":["chat","chat-tools"]}]},{"provider":"gemini","models":[{"id":"models/gemini-test","functions":["chat"]},{"id":"models/gemini-paid","functions":["chat"]}]}],"checked_providers":["groq","gemini"],"fetch_failures":[],"probe_failures":[]}'
resolved="$(sh "$resolve_inventory" "$current" "$all_selected" "$normalized" "$official")"
test "$(printf '%s' "$resolved" | jq -r '.results[] | select(.provider == "groq") | .source')" = "api"
test "$(printf '%s' "$resolved" | jq -r '.results[] | select(.provider == "groq") | .models[0].id')" = "groq-new"
test "$(printf '%s' "$resolved" | jq -r '.results[] | select(.provider == "gemini") | .source')" = "api+agent-filter"
test "$(printf '%s' "$resolved" | jq -r '.results[] | select(.provider == "gemini") | .models | length')" -eq 1
test "$(printf '%s' "$resolved" | jq -r '.results[] | select(.provider == "pollinations") | .abandoned')" = "true"

groq_selected="$(sh "$filter_providers" "$providers" groq)"
unavailable="$(sh "$resolve_inventory" "$current" "$groq_selected" '[]' '{"providers":[],"checked_providers":[],"fetch_failures":[{"provider":"groq","error":"401 Unauthorized"}],"probe_failures":[]}')"
test "$(printf '%s' "$unavailable" | jq -r '.results[0].accepted')" = "false"
test "$(printf '%s' "$unavailable" | jq -r '.preserved_provider_ids[0]')" = "groq"

unchanged="$(sh "$build" "$current" "$unavailable" "2026-07-21T00:00:00Z" "$candidate_dir/unchanged.json")"
test "$(printf '%s' "$unchanged" | jq -r '.generated_at')" = "2026-07-20T00:00:00Z"
test "$(printf '%s' "$unchanged" | jq -r '.providers.groq.models[0].id')" = "verified-chat"

updated="$(sh "$build" "$current" "$resolved" "2026-07-21T00:00:00Z" "$candidate_dir/updated.json")"
test "$(printf '%s' "$updated" | jq -r '.generated_at')" = "2026-07-21T00:00:00Z"
test "$(printf '%s' "$updated" | jq -r '.providers.groq.models[0].id')" = "groq-new"
test "$(printf '%s' "$updated" | jq -r '.providers.gemini.models[0].id')" = "models/gemini-test"
test "$(printf '%s' "$updated" | jq -r '.providers.gemini.models[0].verified_at')" = "2026-07-21T00:00:00Z"

gate="$(sh "$quality_gate" "$current" "$updated" "$resolved" "$valid_candidate" false)"
test "$(printf '%s' "$gate" | jq -r '.approved')" = "true"
test "$(printf '%s' "$gate" | jq -r '.added_model_count')" -eq 2
test "$(printf '%s' "$gate" | jq -r '.removed_model_count')" -eq 2

destructive='{"schema_version":2,"generated_at":"2026-07-21T00:00:00Z","providers":{"groq":{"models":[]},"gemini":{"models":[]}}}'
rejected_gate="$(sh "$quality_gate" "$current" "$destructive" '{"results":[]}' "$valid_candidate" false)"
test "$(printf '%s' "$rejected_gate" | jq -r '.approved')" = "false"
test "$(printf '%s' "$rejected_gate" | jq -r '.removed_model_count')" -eq 2

unselected_change='{"schema_version":2,"generated_at":"2026-07-21T00:00:00Z","providers":{"groq":{"models":[{"id":"verified-chat","functions":["chat"]}]},"gemini":{"models":[]}}}'
unselected_gate="$(sh "$quality_gate" "$current" "$unselected_change" "$unavailable" "$valid_candidate" false)"
test "$(printf '%s' "$unselected_gate" | jq -r '.approved')" = "false"
test "$(printf '%s' "$unselected_gate" | jq -r '.violations[0].provider')" = "gemini"

invalid_gate="$(sh "$quality_gate" "$current" "$updated" "$resolved" '{"valid":false,"error":"provider test model bad has no functions"}' false)"
test "$(printf '%s' "$invalid_gate" | jq -r '.approved')" = "false"
test "$(printf '%s' "$invalid_gate" | jq -r '.candidate_valid')" = "false"

large_models="$(jq -cn '[range(0;51) | {id:("model-" + tostring),functions:["chat"]}]')"
large_candidate="$(jq -cn --argjson models "$large_models" '{schema_version:2,providers:{groq:{models:$models}}}')"
large_inventory="$(jq -cn --argjson models "$large_models" '{results:[{provider:"groq",source:"api",authoritative:true,accepted:true,abandoned:false,models:$models}]}')"
large_gate="$(sh "$quality_gate" '{"schema_version":2,"providers":{}}' "$large_candidate" "$large_inventory" "$valid_candidate" false)"
test "$(printf '%s' "$large_gate" | jq -r '.approved')" = "false"
test "$(printf '%s' "$large_gate" | jq -r '.violations[0].reason' | grep -c 'large change')" -gt 0
large_override="$(sh "$quality_gate" '{"schema_version":2,"providers":{}}' "$large_candidate" "$large_inventory" "$valid_candidate" true)"
test "$(printf '%s' "$large_override" | jq -r '.approved')" = "true"

summary="$(sh "$summarize" all "2026-07-21T00:00:00Z" "$current" "$updated" "$normalized" "$resolved" "$official" "$valid_candidate" "$gate" '{"candidates":[],"rejected_candidate_ids":[],"notes":"fixture"}')"
test "$(printf '%s' "$summary" | jq -r '.quality_gate.approved')" = "true"
test "$(printf '%s' "$summary" | jq -r '.inventory.api_provider_ids[0]')" = "groq"
test "$(printf '%s' "$summary" | jq -r '.inventory.api_agent_provider_ids[0]')" = "gemini"
test "$(printf '%s' "$summary" | jq -r '.manifest.model_count')" -eq 2
test "$(printf '%s' "$summary" | jq -r '.candidate_validation.valid')" = "true"

discovery='{"candidates":[{"id":"groq","base_url":"https://api.groq.com/openai/v1","register_url":"https://console.groq.com/keys","api_key_env":"GROQ_API_KEY","free_basis":"duplicate","source_urls":["https://console.groq.com/docs/rate-limits"],"models":[]},{"id":"new-free","base_url":"https://api.example.com/v1","register_url":"https://example.com/register","api_key_env":"NEW_FREE_API_KEY","free_basis":"Documented free quota.","source_urls":["https://example.com/docs/free-tier"],"models":[]}],"notes":"fixture"}'
candidates="$(sh "$normalize_candidates" "$providers" "$discovery" all)"
test "$(printf '%s' "$candidates" | jq -r '.candidates[0].id')" = "new-free"
test "$(printf '%s' "$candidates" | jq -r '.rejected_candidate_ids[0]')" = "groq"

targeted_candidates="$(sh "$normalize_candidates" "$providers" '{{research-new-providers}}' gemini)"
test "$(printf '%s' "$targeted_candidates" | jq '.candidates | length')" -eq 0
test "$(printf '%s' "$targeted_candidates" | jq -r '.notes')" = "targeted provider run; global provider discovery skipped"

printf 'free-model formula fixture tests passed\n'
