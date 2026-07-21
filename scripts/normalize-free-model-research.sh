#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: normalize-free-model-research.sh <providers-json> <research-json>" >&2
  exit 2
fi

jq -cn --arg providers "$1" --arg research "$2" '
  def decode:
    if type == "string" then
      (try fromjson catch null) as $decoded |
      if ($decoded | type) == "string" then (try ($decoded | fromjson) catch null) else $decoded end
    else . end;
  def supported_policy:
    . == "inventory" or . == "zero-price" or . == "all-listed" or . == "unverified";
  def supported_function:
    . == "chat" or . == "chat-tools" or
    . == "image-understanding" or . == "image-generation" or
    . == "video-understanding" or . == "video-generation" or
    . == "audio-understanding" or . == "speech-to-text" or
    . == "text-to-speech" or . == "embedding" or . == "rerank" or
    . == "moderation";
  def valid_model($provider_sources):
    type == "object" and
    ((.id // "") | type == "string" and length > 0) and
    ((.functions // null) | type == "array" and length > 0) and
    ([.functions[] | supported_function] | all) and
    (((.source_urls // []) | length > 0) or ($provider_sources | length > 0));
  def valid_candidate($provider):
    type == "object" and
    .provider == $provider.id and
    (.policy | supported_policy) and
    ((.models // null) | type == "array") and
    (if .policy == "unverified" then
       (.models | length == 0)
     else
       ((.free_basis // "") | type == "string" and length > 0) and
       ((.source_urls // null) | type == "array" and length > 0) and
       ([.source_urls[] | select(
          type == "string" and
          test("^https://[^/]+(/[^/?#].*|[?][^#].*)$") and
          . != ($provider.register_url // "")
        )] | length > 0) and
       (if .policy == "inventory" then
          (.models | length > 0) and
          ((.source_urls // []) as $provider_sources | [.models[] | valid_model($provider_sources)] | all) and
          (([.models[].id] | length) == ([.models[].id] | unique | length))
        else
          (.models | length == 0)
        end)
     end);
  def normalized($candidate; $id):
    {
      provider: $id,
      policy: $candidate.policy,
      free_basis: ($candidate.free_basis // ""),
      billing_warning: ($candidate.billing_warning // ""),
      source_urls: ($candidate.source_urls // []),
      models: ($candidate.models // []),
      notes: ($candidate.notes // ""),
      accepted: ($candidate.policy != "unverified"),
      validation_error: (if $candidate.policy == "unverified" then "official free-model evidence was not verified" else "" end)
    };
  def rejected($id; $reason):
    {
      provider: $id,
      policy: "unverified",
      free_basis: "",
      billing_warning: "",
      source_urls: [],
      models: [],
      notes: "",
      accepted: false,
      validation_error: $reason
    };

  ($providers | decode) as $provider_list |
  ($research | decode) as $research_list |
  if ($provider_list | type) != "array" then
    error("providers-json must decode to an array")
  elif ($research_list | type) != "array" then
    error("research-json must decode to an array")
  else
    [$provider_list[] as $provider |
      ($provider.id // "") as $id |
      ([$research_list[] | decode | select(type == "object" and .provider == $id)]) as $matches |
      ([$matches[] | select(valid_candidate($provider))]) as $valid |
      if $id == "" then
        error("provider list contains an empty id")
      elif ($valid | length) == 1 then
        normalized($valid[0]; $id)
      elif ($valid | length) > 1 then
        rejected($id; "multiple valid results were returned for the same provider")
      elif ($matches | length) > 0 then
        rejected($id; "provider result did not match the required schema or evidence rules")
      else
        rejected($id; "no result was returned for the provider")
      end]
  end
'
