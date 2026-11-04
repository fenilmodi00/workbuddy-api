# Contributors

This project accepts improvement proposals from the community via Pull Requests. The solutions of the following contributors were adopted and merged into the mainline;
some of those changes were applied manually by the maintainers against the current code baseline, and attribution is preserved here.

## 308532806

- GitHub: https://github.com/308532806
- Proposal: https://github.com/CangShui/workbuddy-gateway/pull/1
- Title: `feat: sync the official model catalog dynamically in /v1/models`
- Adopted: Proposed syncing the model catalog at runtime from the official CLI npm package, to fix the hardcoded `/v1/models`
  list that could not follow new upstream models automatically.
- Implementation: Reimplemented as `models.go` against this repository's code, plus low-traffic and fault-tolerance strategies
  (preferring a single ~20 KB unpkg file, falling back to streaming extraction of the npm tgz capped at 80 MiB,
  exponential backoff for failing versions, a cache with a schema version, and deduplicated merging with the static fallback).
- Since version: v1.9.0

## WorldofPlainness

- GitHub: https://github.com/WorldofPlainness (also submits as `Cooky0606`)
- Proposal: https://github.com/CangShui/workbuddy-gateway/pull/3
- Title: `fix: normalize empty finish_reason in upstream stream chunks`
- Adopted: The upstream sends `finish_reason:""` in every streaming chunk, while the OpenAI spec requires intermediate chunks to use
  `null`; the Anthropic protocol translation layer takes the first non-null value as the final `stop_reason`, so the first empty string
  locks `stop_reason` to `end_turn`, real terminating chunks' `tool_calls` are no longer honored — Claude Code tools stop executing and the
  terminal shows no output while the gateway logs report success. Also cleans up the legacy empty `function_call`
  shells appended by the upstream.
- Implementation: Applied per the original proposal, including the author's `stream_test.go` (chunk normalization, shell removal,
  and end-to-end cases going through the real `handleChatCompletions`).
- Since version: v1.11.0

## xiangshui001

- GitHub: https://github.com/xiangshui001
- Proposal: https://github.com/CangShui/workbuddy-gateway/pull/11
- Title: `fix: harden local gateway auth, credential protection, and request boundaries`
- Adopted:
  - Fixed the npm model-catalog version query canceling the request context before the response body finished reading.
  - Hid the API Key in the startup banner.
  - Used constant-time comparison for the API key, keeping this repository's existing Bearer and `x-api-key` extraction and fallback behavior.
  - Corrected read-only credential validation: an ordinary HTTP 403 no longer immediately marks the token invalid; other account deactivation rules are unchanged.
  - Updated `currAuth` only after credentials were written successfully, keeping the existing file-write approach.
  - Adopted the direction of redacting secret values in logs, adjusted to this repository's needs, preserving error codes, failure reasons, and TraceID;
    original errors used for business decisions and client error responses are not rewritten.
- Implementation: Manually cherry-picked the lower-compatibility-risk parts against the current code and added targeted tests;
  the PR branch was not merged directly, and changes altering startup, request boundaries, or credential-file replacement were not adopted.
- Since version: v1.13.15 (source version)

## MoRanYue

- GitHub: https://github.com/MoRanYue
- Proposal: https://github.com/CangShui/workbuddy-gateway/issues/21
- Title: `occasional 400/11128 "Illegal API invocation from an unapproved channel" after international-site 429 fallback to the domestic site`
- Adopted: Through real outbound request captures and bisection, confirmed that the upstream validates the channel on
  `You are an AI agent powered by DeepSeek Harness` at the start of system messages and returns 11128;
  also verified that leading whitespace, mid-body occurrences, and user messages do not trigger it.
- Implementation: Added `sanitizeprefix.go`, uniformly rewriting that identity sentence at the start of system messages
  into an equivalent neutral prefix at all three protocol entry points, preserving the rest of the body verbatim; added tests for
  boundaries, array content, `developer` normalization, and the three protocol entry points.
- Since version: v1.14.21
- Contributor identity: via GitHub noreply author commit records

## Ricardowindoy

- GitHub: https://github.com/Ricardowindoy
- Proposal: https://github.com/CangShui/workbuddy-gateway/pull/23
- Title: `fix(stream): streaming heartbeat + safe pre-first-byte replay to mitigate edge-node quiet-period RSTs`
- Adopted: Proposed sending SSE comment heartbeats while waiting for the upstream's first valid token, and performing limited replay
  of pre-first-byte RST/EOF when the client has not yet received content; also provided quiet-period measured data, config options, and replay boundaries.
- Implementation: Reimplemented against the current `streamChatResponse` structure, keeping the default 15-second heartbeat and at most one
  replay. Added response-body closing, client-cancel differentiation, scanner non-network-error replay refusal, and handling so that
  a failed replay after the heartbeat was sent ends only with an SSE error event; added concurrency-safe heartbeat and real-HTTP replay regression tests.
- Since version: v1.14.24
- Contributor identity: via GitHub noreply Co-authored-by commit records

## GMagisk9527

- GitHub: https://github.com/GMagisk9527
- Proposal: https://github.com/CangShui/workbuddy-gateway/pull/25
- Title: `fix(compat): complete 11128 compatibility for DSH history, GUI, and config prompts`
- Adopted: Extended sanitization of known DSH identity prefixes to every standard text block in system/assistant,
  config fallback/force injected content, and precisely neutralized the DSH GUI environment sentence; skips fenced, indented, and inline code,
  and never modifies user body text, tool arguments, tool results, reasoning, or images.
- Implementation: Hooked into the current `prepareUpstreamChatRequest` outbound preparation chain shared by all three protocols, running one idempotent pass
  before text-block merging and another after config prompt injection; kept per-block processing before Messages protocol conversion. Reused
  the PR's regression tests, adapted to the current shared preprocessing structure.
- Since version: v1.14.24
- Contributor identity: via GitHub noreply Co-authored-by commit records

## Notes

- All changes above were applied against this repository's code at the time, without directly merging the original branches, while preserving author attribution and source links.
- If your adopted proposal is missing from this file, additions are welcome.
