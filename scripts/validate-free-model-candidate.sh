#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: validate-free-model-candidate.sh <candidate-file>" >&2
  exit 2
fi

candidate_file="$1"
if [ ! -f "$candidate_file" ]; then
  echo "candidate manifest does not exist: $candidate_file" >&2
  exit 2
fi

target_dir="internal/provider"
tmp="$(mktemp "$target_dir/.free-model-candidate.XXXXXX")"
trap 'rm -f "$tmp"' EXIT HUP INT TERM

jq -er '
  if type == "string" then fromjson else . end
  | if type == "array" and length == 1 then .[0].content else . end
  | if type == "string" then fromjson else . end
  | if type == "object" and has("providers") then . else error("expected a model manifest object") end
' "$candidate_file" > "$tmp"

project_root="${FREE_ROUTER_ROOT:-$(pwd)}"
if validation_output="$(go -C "$project_root" run . validate-model-data "$(pwd)/$tmp" 2>&1)"; then
  jq -cn \
    --arg message "$validation_output" \
    --argjson providers "$(jq '.providers | length' "$tmp")" \
    --argjson models "$(jq '[.providers[]?.models[]?] | length' "$tmp")" \
    '{valid:true,error:"",message:$message,provider_count:$providers,model_count:$models}'
else
  status=$?
  concise_error="$(printf '%s\n' "$validation_output" | sed '/^# github\.com\/sjzsdu\/free-router$/d; /^ld: warning:/d; /^exit status [0-9][0-9]*$/d')"
  if [ -z "$concise_error" ]; then
    concise_error="$validation_output"
  fi
  jq -cn \
    --arg error "$concise_error" \
    --argjson status "$status" \
    '{valid:false,error:$error,message:"candidate manifest failed strict validation",exit_code:$status,provider_count:0,model_count:0}'
fi
