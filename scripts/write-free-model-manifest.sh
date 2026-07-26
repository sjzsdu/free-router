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

target="${FREE_ROUTER_MANIFEST_TARGET:-internal/provider/free-models.json}"

if [ "$2" != "true" ]; then
	target_dir="$(dirname -- "$target")"
	mkdir -p "$target_dir"
	tmp="$(mktemp "$target_dir/.free-model-status.XXXXXX")"
	trap 'rm -f "$tmp"' EXIT HUP INT TERM
	jq -s '
	  .[0] as $old | .[1] as $candidate |
	  reduce ($candidate.providers | to_entries[]) as $item
	    ($old;
	      ($item.value.discovery_status // "") as $status |
	      if $status == "" then .
	      else
	        ([.providers[$item.key].models[]?.id] | sort) as $current_ids |
	        ([$item.value.models[]?.id] | sort) as $candidate_ids |
	        .providers[$item.key] = (.providers[$item.key] // {models:[]}) |
	        if $status == "ready" and $current_ids != $candidate_ids then
	          .providers[$item.key].discovery_status = "awaiting-approval" |
	          .providers[$item.key].discovery_message = ("Formula 已发现 " + (($candidate_ids|length)|tostring) + " 个模型，但本次模型变化尚未通过质量门禁")
	        else
	          .providers[$item.key].discovery_status = $status |
	          .providers[$item.key].discovery_message = ($item.value.discovery_message // "")
	        end
	      end)
	' "$target" "$candidate_file" > "$tmp"
	project_root="${FREE_ROUTER_ROOT:-$(pwd)}"
	case "$tmp" in
		/*) validation_path="$tmp" ;;
		*) validation_path="$(pwd)/$tmp" ;;
	esac
	go -C "$project_root" run . validate-model-data "$validation_path" >/dev/null
	if cmp -s "$tmp" "$target"; then
		rm -f "$tmp"
		status_changed=false
	else
		mv -f "$tmp" "$target"
		status_changed=true
	fi
	trap - EXIT HUP INT TERM
	printf '{"directory":"internal/provider","files":["internal/provider/free-models.json"],"changed":false,"applied":false,"status_changed":%s}\n' "$status_changed"
  exit 0
fi

target_dir="$(dirname -- "$target")"
mkdir -p "$target_dir"
tmp="$(mktemp "$target_dir/.free-models.XXXXXX")"
trap 'rm -f "$tmp"' EXIT HUP INT TERM

jq -er '
  def normalize_provider:
    (.discovery_status // "") as $status
    | if $status == "verification-failed" then
        .models = []
      elif $status == "confirmed-empty" then
        .models = []
      elif $status == "ready" then
        if ((.models // []) | length) > 0 then
          .discovery_status = "ready"
        else
          .models = [] | .discovery_status = "confirmed-empty"
        end
      else
        .
      end;
  if type == "string" then fromjson else . end
  | if type == "array" and length == 1 then .[0].content else . end
  | if type == "string" then fromjson else . end
  | if type == "object" and has("providers") then . else error("expected a model manifest object") end
  | .providers |= with_entries(.value |= ((. // {}) | .models = (.models // []) | normalize_provider))
  | . as $candidate
  | reduce (input.providers // {} | to_entries[]) as $item
      ($candidate;
        if .providers[$item.key] == null then
          .providers[$item.key] = $item.value
        else
          .
        end)
' "$candidate_file" "$target" > "$tmp"

project_root="${FREE_ROUTER_ROOT:-$(pwd)}"
case "$tmp" in
	/*) validation_path="$tmp" ;;
	*) validation_path="$(pwd)/$tmp" ;;
esac
go -C "$project_root" run . validate-model-data "$validation_path" >/dev/null
if cmp -s "$tmp" "$target"; then
	rm -f "$tmp"
  changed=false
else
  mv -f "$tmp" "$target"
  changed=true
fi
trap - EXIT HUP INT TERM
printf '{"directory":"internal/provider","files":["internal/provider/free-models.json"],"changed":%s,"applied":true}\n' "$changed"
