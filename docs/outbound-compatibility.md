# Multi-Harness Outbound Compatibility

The three native entry points share the same outbound preparation chain after protocol conversion:

- Chat Completions: `/v1/chat/completions`
- OpenAI Responses: `/v1/responses`
- Anthropic Messages: `/v1/messages`

The goal is to align with the verified official request structure as closely as possible while preserving the client's instruction, tool, and parameter semantics.
**It does not copy the official private system prompt verbatim, and it does not promise to bypass server-side channel validation.**
Parameters and headers are based on desktop samples captured domestically on 2026-10-21 and do not claim to automatically track the latest official site version.

## Enabling and Fallback

**Deploy a new build containing this feature before adding the config field. Older binaries do not recognize requestProfile
and may reject the new config; landing this code does not mean the production service is already updated.**

Add the following field inside the `upstream` object of an existing config, without replacing other settings or credentials:

```json
{
  "upstream": {
    "requestProfile": "official"
  }
}
```

- After config hot reload the setting affects new model requests; no restart needed.
- Switch to `"legacy"` to fall back. Omitted field, empty string, or null means legacy.
- Invalid values or wrong types are rejected by config validation; hot reload keeps the last valid config and never partially applies.
- New example configs use official explicitly; existing configs are never rewritten automatically.
- The profile is selected once per logical request; hot reload never changes the compression or header policy of in-flight requests and their retries.
- This feature does not automatically enable `debug.fullRequests`.

## What Official Compatibility Mode Does

### Unified Body

1. Keeps the existing developer→system handling, known-prefix sanitization, tool-history repair, reasoning-history repair, and global prompt rules.
2. Merges only **consecutive leading plain-text system messages without extra metadata**, preserving text order, and appends the configured forced prompt after merging.
   It never crosses user/assistant/tool boundaries and never drops metadata such as images, name, or cache_control;
   when lossless merging is impossible, the original structure is kept.
3. Adds `stream_options.include_usage=true` when the field is missing, without overriding an explicit false.
4. For `deepseek-v4.1-flash`, which has a complete captured sample:
   - When temperature is unspecified and top_p is unspecified, adds temperature=1;
   - When verbosity is unspecified, adds verbosity=high;
   - When no output budget is declared and the model catalog has no cap yet, adds the verified max_tokens=128000.
5. Chat's max_completion_tokens is converted to max_tokens when max_tokens is absent;
   when both exist, the client's values are never overridden. Clients should still avoid sending conflicting budgets.
6. Unknown models get no guessed temperature, verbosity, or output budget cap.

`null` counts as undeclared; valid explicit `0`, `false`, shorter output budgets, and low/high/max are all preserved.
tool_choice and parallel_tool_calls are not deleted merely because the official sample omits them, and tool names and parameter schemas are not trimmed down to the official 23 tools.
The gateway still uses SSE upstream as before and aggregates locally for non-streaming clients.

### Completing Protocol Parameter Conversion

These conversion fixes do not depend on the official switch:

| Source protocol field | Gateway Chat field |
|---|---|
| Responses reasoning.effort | reasoning_effort, preserving explicit values like max |
| Responses text.verbosity | verbosity |
| Responses parallel_tool_calls | parallel_tool_calls, preserving false |
| Responses max_output_tokens | max_tokens |
| Messages output_config.effort | reasoning_effort, taking priority over legacy thinking.effort and budget derivation |
| Messages thinking.type=disabled | Thinking stays disabled and is not re-enabled by output_config |
| Messages tool_choice.disable_parallel_tool_use | Inverted value of parallel_tool_calls |

No unconditional `high→max` upgrade is performed. A user actively choosing high, or an auxiliary title task using high,
is not the same thing as a fronting proxy wrongly downgrading the effort.

### Domestic Model Headers and Compression

Only domestic model POST `/v2/chat/completions` requests use the captured static headers:

- User-Agent: `WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0`
- Accept: `application/json`
- X-IDE-Type / X-IDE-Name / X-IDE-Version: `WorkBuddy / WorkBuddy / 5.8.3`
- X-Private-Data: `true`
- X-Codebuddy-Request: `1`
- X-Requested-With: `XMLHttpRequest`
- x-stainless-*: aligned with the captured Node SDK static values
- X-Agent-Intent / X-Agent-Purpose / X-Agent-Type: `craft / conversation / main`
- X-Product: `SaaS`
- X-Request-ID and X-Conversation-Message-ID use the same 32-character message ID;
  safe retries of the same logical request reuse that identifier.

The account's access token, user/enterprise/domain info, and session identifiers are preserved;
model requests no longer carry the legacy X-Client-ID, X-Client-Version, Origin, Referer,
X-Refresh-Token, and default X-No-* headers.
Control endpoints such as login, refresh, quota, and the model catalog keep their original logic.

Model JSON is gzip-compressed, with headers consistent with the actual compressed bytes and Content-Length.
Response compression is handled by the Go HTTP client, and decode capabilities such as br/compress are never declared.

On 2026-10-22, `https://copilot.tencent.com/v2/responses` was probed with a real short request
and returned HTTP 404 `Route Not Found`. For now `/v2/chat/completions` remains in use,
and the not-yet-supported Responses upstream path is not switched to.

The following fields are never fabricated:

- Device credentials such as X-Device-Token;
- SDK identities such as x-stainless / Node runtime that do not belong to this Go gateway;
- Agent type, purpose, and business growth events that cannot be reliably determined;
- Other tracing fields such as B3 and ACP that were never captured and verified.

International sites keep legacy UA, auth headers, site, and request encoding; domestic desktop headers are not copied to them.
The official body-preparation rules still apply to those requests; unknown models still get no unverified sampled fields injected.
Domestic model URLs still follow the original per-account site configuration and were not changed to the other domain seen in captures; near-compatibility is not advertised as the identical official link.

### Responses Tool Fidelity

Responses' namespace, custom, and web_search tools are no longer silently dropped:

- Namespace subtools are sent to the Chat upstream as flat function names of the form `namespace__tool`;
- Custom tools (for example `apply_patch`) are sent upstream as functions with a single string `input`;
- web_search is kept as a callable function tool;
- When the upstream returns tool calls, the gateway restores `namespace`, `custom_tool_call`, and
  `response.custom_tool_call_input.*` events per the mapping, so the client sees the original Responses shape;
- `function_call`, `custom_tool_call`, and their outputs in history are converted as well, so follow-up turns lose nothing.

This does not turn the Chat upstream into a full OpenAI Responses implementation; actual tool execution is still done by the client.

### Identity Presentation

Presentation identity replacement in system, developer, and assistant history is configured in the `identity`
section of `config.json`; when omitted, this version's default target `WorkBuddy` is used. `identity:{}`, an empty `target`, or empty `replacements` also run with the defaults and never block startup.

To match the official WorkBuddy wording more closely, set the target to `WorkBuddy`:

```json
{
  "identity": {
    "enabled": true,
    "target": "WorkBuddy"
  }
}
```

For word-by-word control, use `replacements`; keys are the original text and values are the replacements:

```json
{
  "identity": {
    "enabled": true,
    "target": "WorkBuddy",
    "replacements": {
      "Claude Code": "WorkBuddy",
      "Claude": "WorkBuddy",
      "DeepSeek Harness": "WorkBuddy",
      "Codex": "WorkBuddy",
      "ZCode": "WorkBuddy",
      "Grok Build": "WorkBuddy",
      "Grok": "WorkBuddy",
      "Grok released by xAI": "WorkBuddy"
    }
  }
}
```

`enabled:false` fully disables replacement. Rules apply only to system/developer/assistant text;
user, tool, observed reasoning values, tool arguments, and tool results never participate. The official client does not go through the gateway,
so the original text of official direct requests is never rewritten by these rules.
The current implementation is text-level replacement, so product names and skill descriptions in the body are also replaced; it is not limited to the opening identity sentence.

Grok Build and other model labels are handled via the `identity.replacements` config, preserving the request's `model`. This version includes `"You are(.*)released by xAI": "You are WorkBuddy"` by default; fresh installs generate it and upgrades from older configs fill in missing entries on startup, while existing custom values and disabled switches are preserved. It matches only the start of each text block, does not span lines, caps its dynamic part at 256 Unicode characters, and requires non-empty fixed text at both ends; it does not judge client origin or sentence semantics, so ordinary task instructions matching the pattern may also hit.

The legacy DSH identity sentence, the full GUI opening, and two fixed template rewrites have all moved into the default config entries; turning off `identity.enabled` stops those text rewrites too. Ordinary keys run as case-sensitive global literal replacements, and the code snippet containing the GUI sentence is no longer specially protected; user/tool, tool arguments, and reasoning content still never participate. Omitted or empty mappings still use the default rules, and the authoritative defaults are in `config.example.json`. Real upstream acceptance is not yet verified.

## Integration Notes for Codex, dsh, Claude Code, and Grok Build

Have clients connect directly to the gateway's matching native protocol entry point whenever possible, avoiding an extra Responses→Chat or Messages→Chat translation layer in between.

- Codex / dsh using Responses can submit instructions, developer/user, tools, and reasoning.effort as-is.
- Claude Code and others using Messages can submit system, thinking, output_config, and Anthropic tools as-is.
- Native Chat clients should keep the distinction between system/developer and user.

### Streaming Fields and History Return

- Chat's `delta.tool_calls` omits `id`, `type`, and `function.name` when their value is `""`; it keeps `index`, `function.arguments`, and extension fields. Clients accumulate arguments by `index`. Processing is done chunk by chunk and applies to all models and clients. Field definitions: [OpenAI streaming tool-call docs](https://developers.openai.com/api/docs/guides/function-calling#streaming).
- The Messages thinking start event contains `thinking:""` and `signature:""`, followed by `thinking_delta`, `signature_delta`, and `content_block_stop`, following the [Anthropic streaming event example](https://platform.claude.com/docs/en/build-with-claude/streaming#streaming-request-with-thinking). The current end signature is generated by the gateway, used only for this project's compatibility output, and has no Anthropic server-side signature verification capability.
- Chat's non-empty `reasoning_content` passes through unchanged; clients must return the full reasoning history in the next request. DeepSeek's tool-request requirements: [Thinking Mode](https://api-docs.deepseek.com/guides/thinking_mode/). Tool names or reasoning text already lost cannot be recovered by output sanitization; verify with a new session after updating.

Local regression covers interleaved tool chunks, argument stitching, and returning reasoning text and tool results. On 2026-11-01 a synthetic `echo` request was verified with `deepseek-v4.1-flash`: the response from the existing gateway went through the repaired forwarding function, preserved tool names and reasoning content, and produced a correct answer after tool results were returned. That verification ran in an isolated process, without replacing the running gateway or running a full Grok CLI session.

Current Responses tool translation supports fidelity-mapped requests/responses for function,
namespace subtools, custom, and web_search. Actual search results for hosted search are still executed by the client; this mode must not be treated
as a full OpenAI API-equivalent implementation.

This round of diagnostics found cases like "client sent max, but it reached the gateway as high after CPA" and "locally developer, but user by the time it hit the gateway".
The gateway cannot reliably recover the original intent from roles and effort levels already lost:

- It does not force all high to max;
- It does not promote user text to system because it contains `<app-context>`, Codex, or Harness keywords;
- When a fronting proxy is involved, its effort mapping and role translation should be fixed too; changing only this gateway is not enough.

## Logging and Troubleshooting

New events use the existing TraceID and are written to the existing project `logs/`:

- `official_request_body_prepared`: which default fields were added, without recording prompt body text;
- `official_instruction_messages_merged`: the number of merged leading instructions;
- `upstream_request_profile_applied`: which profile was used, whether domestic headers were applied, and byte counts before/after compression;
- Config switches and encoding failures also get persisted audit records.

Only with both enabled and fullRequests explicitly turned on is the full redacted JSON recorded.
In gzip mode, the full outbound log is explicitly marked:

```text
body_representation = decoded_json_before_content_encoding
content_encoding = gzip
wire_body_bytes = actual compressed length
body_bytes = uncompressed JSON length
```

So full messages/tools can be compared directly without mistaking gzip binaries for JSON.
These are snapshots taken before HTTP send, not TLS captures. Turn off fullRequests after troubleshooting; the full business body remains sensitive.

## Validation and Boundaries

New tests use synthetic accounts, instructions, and a local upstream, never touching real model services:

- Six entry points (three protocols × streaming/aggregated) produce identical upstream JSON for the same semantic input;
- Real HTTP receives gzip, usage, and a request ID equal to the message ID;
- high/max/off, explicit false/0, unknown models, and short budgets are preserved;
- Message-merge boundaries, image and schema preservation;
- Hot-reload last-good config, request snapshots, and safe retries;
- Optional offline replay that only reads historical private capture material.

```powershell
go test ./...
go test -race ./...
```

Optional historical capture replay (sends no network requests):

```powershell
$env:WORKBUDDY_COMPAT_CAPTURE = 'C:\private\captured-requests.private.json'
try { go test -run '^TestOfficialPrivateCaptureReplay$' -count=1 -v }
finally { Remove-Item Env:WORKBUDDY_COMPAT_CAPTURE }
```

The fixture must be a fully redacted record with a requests array produced by the diagnostic capture tool.
Original capture files and official private prompts are never committed to the repository. Passing local tests is not production upstream acceptance
and cannot guarantee the server will accept any harness forever.

Protocol parameter references:
[OpenAI Responses parameters](https://developers.openai.com/api/reference/cli/resources/responses/methods/create),
[Anthropic effort](https://platform.claude.com/docs/en/build-with-claude/effort).
The official desktop defaults and static headers come from authorized captures on this machine and cannot be replaced by generic API docs.
