#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: normalize-provider-candidates.sh <providers-json> <discovery-json> <provider|all>" >&2
  exit 2
fi

if [ "$3" != "all" ]; then
  printf '%s\n' '{"candidates":[],"rejected_candidate_ids":[],"notes":"targeted provider run; global provider discovery skipped"}'
  exit 0
fi

jq -cn --arg providers "$1" --arg discovery "$2" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def direct_evidence($register_url):
    type == "string" and
    test("^https://[^/]+(/[^/?#].*|[?][^#].*)$") and
    . != $register_url;
  def valid_candidate($existing):
    (.id // "") as $id |
    (.register_url // "") as $register_url |
    type == "object" and
    ($id | type == "string" and length > 0) and
    ($existing | index($id) | not) and
    ((.base_url // "") | type == "string" and startswith("https://")) and
    ($register_url | type == "string" and startswith("https://")) and
    ((.api_key_env // "") | type == "string" and length > 0) and
    ((.free_basis // "") | type == "string" and length > 0) and
    ((.source_urls // null) | type == "array" and
      ([.[] | select(direct_evidence($register_url))] | length > 0)) and
    ((.models // []) | type == "array");

  ($providers | decode) as $provider_list |
  ($discovery | decode) as $result |
  if ($provider_list | type) != "array" then
    error("providers-json must decode to an array")
  elif ($result | type) != "object" or (($result.candidates // null) | type) != "array" then
    error("discovery-json must contain a candidates array")
  else
    ([$provider_list[].id] | unique) as $existing |
    ([$result.candidates[] | select(valid_candidate($existing))] | unique_by(.id)) as $accepted |
    {
      candidates: $accepted,
      rejected_candidate_ids: [
        $result.candidates[] |
        select((valid_candidate($existing) | not)) |
        (.id // "<missing-id>")
      ],
      notes: ($result.notes // "")
    }
  end
'
