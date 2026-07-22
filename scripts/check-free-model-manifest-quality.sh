#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
  echo "usage: check-free-model-manifest-quality.sh <current-manifest-file-or-json> <candidate-manifest-file-or-json> <resolved-inventory-json> <candidate-validation-json> <allow-large-changes>" >&2
  exit 2
fi

read_json_input() {
  if [ -f "$1" ]; then
    cat "$1"
  else
    printf '%s' "$1"
  fi
}

current="$(read_json_input "$1")"
candidate="$(read_json_input "$2")"

jq -cn --arg current "$current" --arg candidate "$candidate" --arg inventory "$3" --arg validation "$4" --arg allow_large "$5" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def ids($entry): [($entry.models // [])[] | .id] | unique | sort;

  ($current | decode) as $old |
  ($candidate | decode) as $new |
  ($inventory | decode) as $resolved |
  ($validation | decode) as $validated |
  if ($old.providers | type) != "object" or ($new.providers | type) != "object" then
    error("manifest comparison requires provider objects")
  elif ($resolved.results | type) != "array" then
    error("resolved inventory is invalid")
  else
    (($resolved.results | map({key:.provider,value:.}) | from_entries)) as $decisions |
    ([((($old.providers | keys) + ($new.providers | keys) + ($decisions | keys)) | unique)[] as $id |
      (ids($old.providers[$id] // {})) as $before |
      (ids($new.providers[$id] // {})) as $after |
      ($decisions[$id] // null) as $decision |
      {
        provider:$id, before:($before|length), after:($after|length),
        added:($after-$before), removed:($before-$after),
        source:($decision.source // "preserved"), abandoned:($decision.abandoned // false),
        violation: (
          if $decision == null then
            if $before == $after then "" else "unselected provider changed" end
          elif $decision.accepted != true or $decision.authoritative != true then
            if $before == $after then "" else "provider changed after non-authoritative discovery failure" end
          elif $decision.abandoned == true then
            if ($after|length) == 0 then "" else "abandoned provider retains models in candidate manifest" end
          elif $after != ([$decision.models[]?.id] | unique | sort) then
            "candidate inventory does not match authoritative discovery result"
          else ""
          end)
      }]) as $checks |
    ([$checks[] | select((.added|length)>0 or (.removed|length)>0)]) as $changes |
    ([$checks[] | select(.violation != "") | {provider,reason:.violation}]) as $source_violations |
    (if $validated.valid == true then [] else [{provider:"manifest",reason:("candidate manifest is invalid: " + ($validated.error // "strict validation failed"))}] end) as $schema_violations |
    (if $allow_large == "true" then [] else
      ([$changes[] |
        if (.added|length) > 50 then {provider,reason:("large change requires explicit approval: " + ((.added|length)|tostring) + " models added")}
        elif (.removed|length) > 20 then {provider,reason:("large change requires explicit approval: " + ((.removed|length)|tostring) + " models removed")}
        else empty end] +
       (if ([$changes[].added[]?] | length) > 100 then [{provider:"manifest",reason:"large change requires explicit approval: more than 100 models added globally"}] else [] end) +
       (if ([$changes[].removed[]?] | length) > 50 then [{provider:"manifest",reason:"large change requires explicit approval: more than 50 models removed globally"}] else [] end))
     end) as $size_violations |
    ($source_violations + $schema_violations + $size_violations) as $violations |
    {
      approved:(($violations|length)==0), changed:(($changes|length)>0),
      candidate_valid:($validated.valid == true), large_changes_allowed:($allow_large == "true"),
      before_model_count:([$old.providers[].models[]?]|length),
      after_model_count:([$new.providers[].models[]?]|length),
      added_model_count:([$changes[].added[]?]|length),
      removed_model_count:([$changes[].removed[]?]|length),
      changes:$changes, violations:$violations,
      rejection_reasons:[$violations[] | .provider + ": " + .reason]
    }
  end
'
