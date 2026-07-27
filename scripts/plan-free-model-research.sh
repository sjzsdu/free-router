#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: plan-free-model-research.sh <provider-selection-json> <official-catalog-json>" >&2
  exit 2
fi

jq -cn --arg selection "$1" --arg catalog "$2" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def api_checked($catalog; $id):
    (($catalog.checked_providers // []) | index($id)) != null;
  def api_models($catalog; $id):
    [$catalog.providers[]? | select(.provider == $id) | .models[]?];
  def research_reason($provider; $catalog):
    ($provider.id // "") as $id |
    ($provider.model_discovery // "api") as $mode |
    if $mode == "agent" then "agent-source"
    elif $mode == "api-agent-filter" then "free-policy-filter"
    elif (api_checked($catalog; $id) | not) then "api-unavailable-fallback"
    elif (api_models($catalog; $id) | length) == 0 then "empty-api-fallback"
    else "api-catalog-sufficient"
    end;

  ($selection | decode) as $selected |
  ($catalog | decode) as $official |
  if ($selected.providers | type) != "array" then
    error("provider selection is invalid")
  elif ($official | type) != "object" then
    error("official catalog result is invalid")
  else
    [$selected.providers[] |
      . as $provider |
      (research_reason($provider; $official)) as $reason |
      {
        provider: $provider.id,
        research: ($reason != "api-catalog-sufficient"),
        reason: $reason
      }] as $providers |
    ({providers: $providers} +
      (reduce $providers[] as $provider ({};
        .["research_" + ($provider.provider | gsub("-"; "_"))] = $provider.research)))
  end
'
