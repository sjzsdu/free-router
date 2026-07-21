#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: apply-probed-inventory.sh <manifest-json> <discovery-json> <timestamp>" >&2
  exit 2
fi

jq -cn --arg manifest "$1" --arg discovery "$2" --arg timestamp "$3" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  ($manifest | decode) as $current |
  ($discovery | decode) as $result |
  if ($current | type) != "object" or ($current.providers | type) != "object" then
    error("manifest is invalid")
  elif ($result | type) != "object" or (($result.providers // null) | type) != "array" then
    error("discovery result is invalid")
  else
    ($current.providers | with_entries(
      .value |= (
        if .policy == "inventory" then .policy = "unverified" else . end |
        del(.models)
      )
    )) as $without_stale_inventory |
    (reduce $result.providers[] as $provider
      ($without_stale_inventory;
       if (.[$provider.provider] != null) and (($provider.models // []) | length > 0) then
         .[$provider.provider].policy = "inventory" |
         .[$provider.provider].models = [
           $provider.models[] |
           .verified_at = $timestamp
         ]
       else . end)) as $providers |
    {
      schema_version: 1,
      generated_at: (if $providers == $current.providers then $current.generated_at else $timestamp end),
      providers: $providers
    }
  end
'
