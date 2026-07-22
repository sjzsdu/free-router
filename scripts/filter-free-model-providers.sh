#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: filter-free-model-providers.sh <providers-json> <provider|all>" >&2
  exit 2
fi

jq -cn --arg providers "$1" --arg target "$2" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  ($providers | decode) as $all |
  if ($all | type) != "array" then
    error("provider list is invalid")
  else
    (if $target == "all" then $all else [$all[] | select(.id == $target)] end) as $selected |
    if ($selected | length) == 0 then
      error("unknown provider: " + $target)
    else
      ({target: $target, all: ($target == "all"), providers: $selected} +
       (reduce $selected[] as $provider ({};
         .[$provider.id] = true |
         .["research_" + ($provider.id | gsub("-"; "_"))] =
           (($provider.model_discovery == "agent") or ($provider.model_discovery == "api-agent-filter")))))
    end
  end
'
