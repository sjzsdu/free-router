#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: resolve-free-model-inventory.sh <current-manifest-json> <provider-selection-json> <normalized-research-json> <official-catalog-json>" >&2
  exit 2
fi

jq -cn --arg current "$1" --arg selection "$2" --arg research "$3" --arg catalog "$4" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def api_models($catalog; $id):
    [$catalog.providers[]? | select(.provider == $id) | .models[]?];
  def api_checked($catalog; $id):
    (($catalog.checked_providers // []) | index($id)) != null;
  def research_for($research; $id):
    ([$research[] | select(.provider == $id)] | first // {provider:$id, accepted:false, models:[], validation_error:"agent returned no usable inventory"});
  def intersect($proposal; $official):
    [$proposal.models[]? as $candidate |
      ($official | map(select(.id == $candidate.id)) | first // null) as $api |
      select($api != null) |
      ($api * $candidate)];
  def failure_for($catalog; $id):
    ([$catalog.fetch_failures[]? | select(.provider == $id) | .error] | first // "official model catalog was not available");

  ($current | decode) as $manifest |
  ($selection | decode) as $selected |
  ($research | decode) as $agent |
  ($catalog | decode) as $official |
  if ($manifest.providers | type) != "object" then
    error("current manifest is invalid")
  elif ($selected.providers | type) != "array" then
    error("provider selection is invalid")
  elif ($agent | type) != "array" then
    error("normalized research must be an array")
  elif ($official | type) != "object" then
    error("official catalog result must be an object")
  else
    [$selected.providers[] as $provider |
      ($provider.id // "") as $id |
      ($provider.model_discovery // "api") as $mode |
      (api_models($official; $id)) as $api |
      (research_for($agent; $id)) as $proposal |
      (intersect($proposal; $api)) as $filtered |
      if $mode == "agent" then
        {
          provider:$id, source:"agent", authoritative:true,
          accepted:true, abandoned:($proposal.accepted != true or (($proposal.models // []) | length) == 0),
          models:(if $proposal.accepted == true then ($proposal.models // []) else [] end),
          free_basis:($proposal.free_basis // $provider.free_basis // ""),
          billing_warning:($proposal.billing_warning // $provider.billing_warning // ""),
          source_urls:($proposal.source_urls // $provider.source_urls // []),
          message:(if $proposal.accepted == true then "agent inventory accepted" else ($proposal.validation_error // "agent inventory unavailable; provider abandoned") end)
        }
      elif (api_checked($official; $id) | not) then
        {
          provider:$id, source:"api", authoritative:false, accepted:false, abandoned:false, models:[],
          free_basis:($provider.free_basis // ""), billing_warning:($provider.billing_warning // ""), source_urls:($provider.source_urls // []),
          message:failure_for($official; $id)
        }
      elif $mode == "api-agent-filter" then
        {
          provider:$id, source:"api+agent-filter", authoritative:true,
          accepted:true, abandoned:($proposal.accepted != true or ($filtered | length) == 0),
          models:(if $proposal.accepted == true then $filtered else [] end),
          free_basis:($proposal.free_basis // $provider.free_basis // ""),
          billing_warning:($proposal.billing_warning // $provider.billing_warning // ""),
          source_urls:($proposal.source_urls // $provider.source_urls // []),
          message:(if $proposal.accepted != true then "agent could not identify free models from official catalog; provider abandoned" elif ($filtered | length) == 0 then "agent models did not occur in official catalog; provider abandoned" else "official catalog filtered by free-policy agent" end)
        }
      else
        {
          provider:$id, source:"api", authoritative:true, accepted:true,
          abandoned:(($api | length) == 0), models:$api,
          free_basis:($provider.free_basis // ""), billing_warning:($provider.billing_warning // ""), source_urls:($provider.source_urls // []),
          message:(if ($api | length) == 0 then "official catalog contains no eligible free models; provider abandoned" else "official catalog accepted" end)
        }
      end]
    | . as $results
    | {
        results:$results,
        model_count:([$results[] | select(.accepted == true and .abandoned != true) | .models[]?] | length),
        updated_provider_ids:[$results[] | select(.accepted == true and .abandoned != true) | .provider],
        abandoned_provider_ids:[$results[] | select(.accepted == true and .abandoned == true) | .provider],
        preserved_provider_ids:[$results[] | select(.accepted != true) | .provider]
      }
  end
'
