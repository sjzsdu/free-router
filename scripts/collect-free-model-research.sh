#!/bin/sh
set -eu

# Conditional Formula steps that were skipped remain as literal {{step-id}}
# template strings. Decode completed agent/script outputs and discard only those
# unresolved placeholders; provider/schema validation remains the normalizer's job.
jq -cn --args '
  def decode:
    if type != "string" then .
    elif startswith("{{") and endswith("}}") then null
    else
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then
        (try ($decoded | fromjson) catch null)
      elif ($decoded | type) == "object" and (($decoded.stdout // null) | type) == "string" then
        (try ($decoded.stdout | fromjson) catch null)
      else $decoded end
    end;
  [$ARGS.positional[] | decode | select(type == "object")]
' "$@"
