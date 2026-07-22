#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: write-free-model-manifest.sh <candidate-file> <approved>" >&2
  exit 2
fi

candidate_file="$1"
if [ ! -f "$candidate_file" ]; then
  echo "candidate manifest does not exist: $candidate_file" >&2
  exit 2
fi

if [ "$2" != "true" ]; then
  printf '{"directory":"internal/provider","files":["internal/provider/free-models.json"],"changed":false,"applied":false}\n'
  exit 0
fi

target="internal/provider/free-models.json"
tmp="$(mktemp "internal/provider/.free-models.XXXXXX")"
trap 'rm -f "$tmp"' EXIT HUP INT TERM

jq -er '
  if type == "string" then fromjson else . end
  | if type == "array" and length == 1 then .[0].content else . end
  | if type == "string" then fromjson else . end
  | if type == "object" and has("providers") then . else error("expected a model manifest object") end
' "$candidate_file" > "$tmp"

project_root="${FREE_ROUTER_ROOT:-$(pwd)}"
go -C "$project_root" run . validate-model-data "$(pwd)/$tmp"
if cmp -s "$tmp" "$target"; then
	rm -f "$tmp"
  changed=false
else
  mv -f "$tmp" "$target"
  changed=true
fi
trap - EXIT HUP INT TERM
printf '{"directory":"internal/provider","files":["internal/provider/free-models.json"],"changed":%s,"applied":true}\n' "$changed"
