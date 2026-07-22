#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: project-provider-model-catalog.sh <official-catalog-json> <provider>" >&2
  exit 2
fi

jq -cn --arg catalog "$1" --arg provider "$2" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  ($catalog | decode) as $source |
  {
    provider:$provider,
    available:(($source.available[$provider] // false) == true),
    models:[
      $source.providers[]? |
      select(.provider == $provider) |
      .models[]? |
      {
        id,
        functions:(.functions // [])
      }
    ]
  }
'
