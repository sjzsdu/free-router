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
  def supported_function:
    . == "chat" or . == "chat-tools" or
    . == "image-understanding" or . == "image-generation" or
    . == "video-understanding" or . == "video-generation" or
    . == "audio-understanding" or . == "speech-to-text" or
    . == "text-to-speech" or . == "embedding" or . == "rerank" or
    . == "moderation";
  def url_host:
    capture("^https://(?<host>[^/:?#]+)").host | ascii_downcase;
  def official_hosts($provider):
    [($provider.source_urls // [])[], ($provider.register_url // empty)] |
    map(select(type == "string") | (try url_host catch "")) |
    map(select(length > 0)) |
    unique;
  def official_url($hosts):
    if type != "string" or (test("^https://") | not) then false
    else
      (try url_host catch "") as $host |
      [$hosts[] | select(. == $host or ($host | endswith("." + .)))] | length > 0
    end;
  def valid_model($provider_sources; $official_hosts):
    type == "object" and
    ((.id // "") | type == "string" and length > 0) and
    ((.functions // null) | type == "array" and length > 0) and
    ([.functions[] | supported_function] | all) and
    (((.source_urls // []) | length > 0) or ($provider_sources | length > 0)) and
    ([.source_urls[]? | official_url($official_hosts)] | all);
  def valid_candidate($provider):
    official_hosts($provider) as $official_hosts |
    type == "object" and
    .provider == $provider.id and
    ((.models // null) | type == "array" and length > 0) and
    ((.free_basis // "") | type == "string" and length > 0) and
    ((.source_urls // null) | type == "array" and length > 0) and
    ([.source_urls[] | select(
       type == "string" and
       test("^https://[^/]+(/[^/?#].*|[?][^#].*)$") and
       . != ($provider.register_url // "") and
       official_url($official_hosts)
     )] | length > 0) and
    ([.source_urls[] | official_url($official_hosts)] | all) and
    ((.source_urls // []) as $provider_sources | [.models[] | valid_model($provider_sources; $official_hosts)] | all) and
    (([.models[].id] | length) == ([.models[].id] | unique | length));
  def normalized($candidate; $id):
    {
      provider: $id,
      free_basis: ($candidate.free_basis // ""),
      billing_warning: ($candidate.billing_warning // ""),
      source_urls: ($candidate.source_urls // []),
      models: ($candidate.models // []),
      notes: ($candidate.notes // ""),
      accepted: true,
      validation_error: ""
    };
  def rejected($id; $reason):
    {
      provider: $id,
      free_basis: "",
      billing_warning: "",
      source_urls: [],
      models: [],
      notes: "",
      accepted: false,
      validation_error: $reason
    };

  ($providers | decode) as $provider_selection |
  (if ($provider_selection | type) == "object" then
     ($provider_selection.providers // null)
   else
     $provider_selection
   end) as $provider_list |
  ($research | decode) as $research_list |
  if ($provider_list | type) != "array" then
    error("providers-json must decode to an array")
  elif ($research_list | type) != "array" then
    error("research-json must decode to an array")
  else
    [$provider_list[] |
      . as $provider |
      ($provider.id // "") as $provider_id |
      select(
        .model_discovery == "agent" or
        .model_discovery == "api-agent-filter" or
        ([$research_list[] | decode | select(type == "object" and .provider == $provider_id)] | length) > 0
      )] as $research_providers |
    [$research_providers[] as $provider |
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
