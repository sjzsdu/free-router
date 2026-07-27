#!/bin/sh
set -eu

if [ "$#" -ne 10 ]; then
  echo "usage: summarize-free-model-run.sh <target> <attempted-at> <current-file-or-json> <candidate-file-or-json> <research> <inventory> <official-catalog> <candidate-validation> <quality> <new-providers>" >&2
  exit 2
fi

read_json_input() {
  if [ -f "$1" ]; then
    cat "$1"
  else
    printf '%s' "$1"
  fi
}

current="$(read_json_input "$3")"
candidate="$(read_json_input "$4")"

jq -cn \
  --arg target "$1" --arg attempted_at "$2" --arg current "$current" --arg candidate "$candidate" \
  --arg research "$5" --arg inventory "$6" --arg catalog "$7" --arg validation "$8" --arg quality "$9" --arg new_providers "${10}" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  ($current | decode) as $old |
  ($candidate | decode) as $new |
  ($research | decode) as $checked |
  ($inventory | decode) as $resolved |
  ($catalog | decode) as $official |
  ($validation | decode) as $validated |
  ($quality | decode) as $gate |
  ($new_providers | decode) as $discovery |
  ($resolved.preserved_provider_ids // []) as $preserved_ids |
  {
    target_provider: $target,
    attempted_at: $attempted_at,
    manifest_applied: ($gate.approved == true and $gate.changed == true),
    candidate_validation: {
      valid: ($validated.valid // false),
      error: ($validated.error // ""),
      provider_count: ($validated.provider_count // 0),
      model_count: ($validated.model_count // 0)
    },
    quality_gate: {
      approved: ($gate.approved // false),
      rejection_reasons: ($gate.rejection_reasons // []),
      before_model_count: ($gate.before_model_count // 0),
      after_model_count: ($gate.after_model_count // 0),
      added_model_count: ($gate.added_model_count // 0),
      removed_model_count: ($gate.removed_model_count // 0),
      provider_changes: ($gate.changes // [])
    },
    research: {
      accepted_provider_ids: [$checked[] | select(.accepted == true) | .provider],
      rejected: [$checked[] | select(.accepted != true) | . as $item | select(($preserved_ids | index($item.provider)) == null) | {provider, reason:.validation_error}]
    },
    inventory: {
      model_count: ($resolved.model_count // 0),
      api_provider_ids: [$resolved.results[] | select(.accepted == true and .abandoned != true and .source == "api") | .provider],
      api_agent_provider_ids: [$resolved.results[] | select(.accepted == true and .abandoned != true and .source == "api+agent-filter") | .provider],
      agent_provider_ids: [$resolved.results[] | select(.accepted == true and .abandoned != true and .source == "agent") | .provider],
      agent_fallback_provider_ids: [$resolved.results[] | select(.accepted == true and .abandoned != true and .source == "agent-fallback") | .provider],
      abandoned: [$resolved.results[] | select(.accepted == true and .abandoned == true) | {provider,source,message}],
      preserved: [$resolved.results[] | select(.accepted != true) | {provider,message}]
    },
    official_catalog: {
      checked_provider_ids: ($official.checked_providers // []),
      fetch_failures: ($official.fetch_failures // [])
    },
    new_providers: {
      candidate_ids: [($discovery.candidates // [])[] | .id],
      rejected_candidate_ids: ($discovery.rejected_candidate_ids // []),
      notes: ($discovery.notes // "")
    },
    manifest: {
      generated_at: ($new.generated_at // $old.generated_at // ""),
      provider_count: (($new.providers // {}) | length),
      model_count: ([$new.providers[].models[]?] | length),
      providers_with_models: ([$new.providers[] | select((.models // []) | length > 0)] | length)
    }
  }
'
