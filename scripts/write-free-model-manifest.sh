#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: write-free-model-manifest.sh <manifest-json>" >&2
  exit 2
fi

target="internal/provider/free-models.json"
tmp="$(mktemp "internal/provider/.free-models.XXXXXX")"
trap 'rm -f "$tmp"' EXIT HUP INT TERM

printf '%s' "$1" | jq -er '
  if type == "string" then fromjson else . end
  | if type == "array" and length == 1 then .[0].content else . end
  | if type == "string" then fromjson else . end
  | if type == "object" and has("providers") then . else error("expected a model manifest object") end
' > "$tmp"

project_root="${FREE_ROUTER_ROOT:-$(pwd)}"
go -C "$project_root" run . validate-model-data "$(pwd)/$tmp" >/dev/null
if cmp -s "$tmp" "$target"; then
	rm -f "$tmp"
  changed=false
else
  mv -f "$tmp" "$target"
  changed=true
fi
trap - EXIT HUP INT TERM
printf '{"directory":"internal/provider","files":["internal/provider/free-models.json"],"changed":%s}\n' "$changed"
