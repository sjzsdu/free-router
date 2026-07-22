#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: build-free-model-manifest.sh <current-manifest-json> <resolved-inventory-json> <timestamp> <candidate-file>" >&2
  exit 2
fi

candidate_file="$4"
candidate_dir="$(dirname -- "$candidate_file")"
mkdir -p "$candidate_dir"
tmp="$(mktemp "$candidate_dir/.free-model-candidate.XXXXXX")"
trap 'rm -f "$tmp"' EXIT HUP INT TERM

jq -cn --arg current "$1" --arg research "$2" --arg timestamp "$3" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def compact_object:
    with_entries(select(.value != null and .value != "" and .value != []));
  def entry_from($result):
    ({
      free_basis: $result.free_basis,
      billing_warning: $result.billing_warning,
      source_urls: $result.source_urls,
      models: [$result.models[] | .verified_at = $timestamp] | sort_by(.id)
    } | compact_object);

  ($current | decode) as $manifest |
  ($research | decode) as $inventory |
  if ($manifest | type) != "object" or ($manifest.providers | type) != "object" then
    error("current manifest is invalid")
  elif ($inventory.results | type) != "array" then
    error("resolved inventory must contain a results array")
  else
    (reduce $inventory.results[] as $result
      ($manifest.providers;
       if $result.accepted == true and $result.authoritative == true and $result.abandoned == true then
         del(.[$result.provider])
       elif $result.accepted == true and $result.authoritative == true then
         .[$result.provider] = entry_from($result)
       else
         .
       end)) as $providers |
    ($manifest.providers | map_values(compact_object)) as $old_providers |
    ($providers | map_values(compact_object)) as $new_providers |
    {
      schema_version: 2,
      generated_at: (if $new_providers == $old_providers then $manifest.generated_at else $timestamp end),
      providers: $new_providers
    }
  end
' > "$tmp"

mv -f "$tmp" "$candidate_file"
trap - EXIT HUP INT TERM
cat "$candidate_file"
