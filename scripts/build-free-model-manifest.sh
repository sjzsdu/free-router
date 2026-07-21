#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: build-free-model-manifest.sh <current-manifest-json> <normalized-research-json> <timestamp>" >&2
  exit 2
fi

jq -cn --arg current "$1" --arg research "$2" --arg timestamp "$3" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def compact_object:
    with_entries(select(.value != null and .value != "" and .value != []));
  def safe_transition($old; $new):
    ($old == null) or
    ($new == "inventory") or
    ($new == "zero-price" and ($old.policy == "zero-price" or $old.policy == "all-listed" or $old.policy == "unverified")) or
    ($new == "all-listed" and ($old.policy == "all-listed" or $old.policy == "unverified"));
  def entry_from($result):
    ({
      policy: $result.policy,
      free_basis: $result.free_basis,
      billing_warning: $result.billing_warning,
      source_urls: $result.source_urls,
      models: (if $result.policy == "inventory" then
                 [$result.models[] | .verified_at = (if (.verified_at // "") == "" then $timestamp else .verified_at end)]
               else [] end)
    } | compact_object);

  ($current | decode) as $manifest |
  ($research | decode) as $results |
  if ($manifest | type) != "object" or ($manifest.providers | type) != "object" then
    error("current manifest is invalid")
  elif ($results | type) != "array" then
    error("normalized research must be an array")
  else
    (reduce $results[] as $result
      ($manifest.providers;
       . as $providers |
       ($providers[$result.provider] // null) as $old |
       if ($result.accepted == true) and safe_transition($old; $result.policy) then
         .[$result.provider] = (($old // {}) * entry_from($result) | compact_object)
       else
         .
       end)) as $providers |
    ($manifest.providers | map_values(compact_object)) as $old_providers |
    ($providers | map_values(compact_object)) as $new_providers |
    {
      schema_version: 1,
      generated_at: (if $new_providers == $old_providers then $manifest.generated_at else $timestamp end),
      providers: $new_providers
    }
  end
'
