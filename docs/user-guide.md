# WorkBuddy Local Gateway

> Back to [Project Home](../README.md) · [Directory & Naming Conventions](project-layout.md) · [Testing Notes](../tests/README.md)
>
> This is the complete user manual. All source build and test commands below run from the project root, not from inside `docs/`.

<img width="2836" height="1858" alt="image" src="https://github.com/user-attachments/assets/ed6a66cf-9e3d-47f9-bfe8-ec4895a10a07" />


<img width="917" height="754" alt="image" src="https://github.com/user-attachments/assets/7dcfc461-1357-4991-9565-279047687898" />


A local AI proxy gateway built on the Tencent **CodeBuddy** protocol: **pure Go, zero CGO dependencies, cross-platform single binary**. The Web UI is off by default; login, credential renewal, and service control are all done via the command line (CLI). An optional web console can be enabled (`-webui`, see below).

**v1.14.0**: Adds config hot reload, global account and model web toggles, credential JSON editing, port settings, and log retention/cleanup. Old configs without a default log retention value are not auto-deleted. Upgrade the binary first, then add the new config fields.

**v1.14.1**: The web credentials page has been removed; a new read-only scheduled tasks time page is added; execution records come from the existing backend logs, with no new scheduling module. High-frequency task logs are aggregated per minute, and status changes are recorded immediately.

**v1.14.38**: The web run status and alert rows are displayed compactly, with full alerts moved to hover/touch details; the newest log lines are pinned to the top, tabs are evenly split and capped on screens over 4K. The CLI account table tightens column widths to fit content. API/Web ports support hot switching while preserving existing connections and streams; the previous run state is retained when port binding or debug-log preparation fails, and a data race between status queries and port updates is fixed.

**v1.14.39**: Web polling reuses account/model rows by stable keys and only updates changed cells; identical statistics reuse charts. Operation results are now shown in an overlay that does not shift the page, fixing dark-theme success toasts turning red and toasts being cleared immediately by unrelated polling.

**v1.14.40**: The default `identity` mapping now includes `workspace root → workspace base` and `are paths the user → are paths that the user`. New installs generate a complete default config; upgrading old configs only fills in missing rules and never overwrites existing custom values or re-enables a disabled `identity`.

**v1.14.41**: Removes the duplicate `promptCompatibility` replacement module; text replacement now uniformly uses `identity.replacements`, with the seven default rules and hot reload unchanged. If an old config still has that field, it is read for compatibility and reported as ignored — it will not block startup or keep executing the old rules.

**v1.14.42**: Removes Grok Build's generic automatic identity rewrite of `You are <any tag> released by xAI.`; text replacement is restored to execute only the explicit entries in `identity.replacements`. Grok static default rules, the Chat streaming tool field fix, and the Messages thinking leading-field fix are retained.

**v1.14.43**: The old DSH identity sentence, GUI environment sentence, and two fixed template rewrites are unified into `identity.replacements`; there is no longer a separate text-rewrite bypass. The default config adds `"You are(.*)released by xAI": "You are WorkBuddy"`; new installs generate it and upgrades auto-fill missing entries at startup, while existing custom values and `enabled:false` are preserved. Only one wildcard is allowed, leading/trailing fixed text cannot be empty, no cross-line matching, and the dynamic part is limited to 256 Unicode characters. `identity.enabled:false` turns off all of these text replacements; protocol structure handling such as message roles is unaffected.

**Grok Build compatibility**: `identity` can replace Grok Build, Grok, and full model-tag entries per explicit configuration; the request `model` is kept as-is.

**v1.14.24**: Chat streaming responses gain an SSE heartbeat before the first byte and at most one safe replay; the DSH identity sentence and GUI environment note now also cover assistant history, subsequent text blocks, and config-injected prompts. Enabled by default; replay can be disabled via the `upstream` config.

**v1.14.25**: Responses namespace subtools, custom tools (such as `apply_patch`), and web_search are no longer silently dropped; the `custom_tool_call` and namespace output shapes are restored. Outbound headers now include the collected WorkBuddy desktop static identity; Codex, DeepSeek Harness, Claude Code, and ZCode display names in system/developer/assistant are unified to `Assistant`. Tested `/v2/responses` returns 404, so the upstream path remains `/v2/chat/completions`.

**v1.14.26**: A named `tool_choice` is now sent as the `required` string accepted by upstream and only the selected tool is kept; this fixes namespace/custom forced tool calls being rejected by upstream with 11101.

**v1.14.27**: Domestic model requests now use `https://www.workbuddy.cn/v2/chat/completions`; authentication, refresh, and quota still go through the original `copilot.tencent.com` / `codebuddy.cn`, unaffected by ChatBase. The international site keeps `www.workbuddy.ai`; in testing, international token requests to `copilot.tencent.com` returned 401.

**v1.14.28**: Display identity replacement supports `config.json`'s `identity.enabled`, `identity.target`, and custom `identity.replacements`.

**v1.14.29**: On first startup, new users automatically get a `config.json` containing the full identity mapping; old configs missing identity are auto-supplemented with other fields preserved. When identity is empty, the field is omitted, or target is empty, the default WorkBuddy is used without blocking startup. `replacements` supports any user-defined words, e.g. `"kimi": "WorkBuddy"`.

**v1.14.38**: `gateway.apiPort` / `gateway.webPort` are also hot-reloaded: switching happens only after all new listeners bind successfully, and existing connections and streaming responses finish normally. The web overview compresses run-status height and turns alert bodies into hover details; stats badges and summary card decorations are removed; the newest run-log line is pinned to the top; tabs are centered at 5/6 of the screen width at 4K and below and capped above 4K. The CLI status table tightens column widths to actual content; overly long account and file names are still truncated and stay aligned.

**Both upstream sites are supported** (the same `/v2/plugin/*` protocol; credentials are isolated per site, and account pools can mix both):

| Site | Upstream | Login method | Login command |
|---|---|---|---|
| Domestic site | `copilot.tencent.com` / `www.codebuddy.cn` | WeChat / WeCom QR scan | `login` |
| International site | `www.workbuddy.ai` | In-browser login (email / verification code / SSO) | `login -intl` |

---

## Table of Contents

- [Core Features](#core-features)
- [Command Overview](#command-overview)
- [serve](#serve)
- [login](#login)
- [status](#status)
- [refresh](#refresh)
- [monitor](#monitor)
- [webui (Web Console)](#webui-web-console)
- [probe](#probe)
- [reset](#reset)
- [version / help](#version--help)
- [Multi-Account Pool](#multi-account-pool)
- [Model List and Multipliers](#model-list-and-multipliers)
- [Client Integration](#client-integration)
- [Platform Deployment](#platform-deployment)
- [Security Notes](#security-notes)
- [Building from Source](#building-from-source)

---

## Core Features

- **Dual-site domestic/international reverse proxy**: both sites use the same protocol; credentials are distinguished by the `edition` field, and refresh and chat requests are routed to their own upstream automatically.
- **Full model passthrough**: whatever `model` the client sends is relayed to upstream unchanged, with no whitelist. `/v1/models` is only for client auto-completion and does not affect actual forwarding.
- **Model list merged from two sources**: live API + static npm catalog, deduplicated by ID with the API taking priority; falls back to the local cache on failure. If both fail and there is no cache, that site shows no models this round (calls are unaffected).
- **Model multiplier and price probing**: when a promo is active, `credits` × factor is shown; probing is off by default, and normal model requests still learn free/paid status. Only passing `-allow-price-probes` explicitly enables background probing; real generation requests may consume credits.
- **Multi-account pool + round-robin load balancing**: comma-separated `-auth` or a `-auth-dir` directory; requests are dispatched round-robin. Domestic and international accounts can be mixed.
- **Model-level isolation**: `6004` only cools down the account + model that triggered it; `14018` only blocks that account's current paid model — one model no longer drags down an entire account.
- **Free-site priority**: if a model is "free on one site, paid on the other", accounts on the free site are preferred until they are restricted; when both sites are paid (only multipliers differ), no bias is applied and normal round-robin continues.
- **Free/paid learning**: learned per "account + model" from the response's `usage.credit`; a model is only judged free when `credit=0` and the sample is large enough (`total_tokens ≥ 100`), avoiding false verdicts from small samples.
- **Automatic daily checkin for the domestic site**: during normal operation, every domestic personal account independently checks in at a random time between `08:00–09:30` Beijing time each day. If started or restarted after `08:00`, accounts still pending checkin today that have not yet attempted start immediately, one after another, with at least a 1-minute gap after each preceding request. Failed accounts still retry only once during `09:30–10:00`; credential hot reload does not directly trigger makeup checkins, and international and enterprise accounts are skipped.
- **Credential hot reload (no restart)**: credential sources are scanned every 5 seconds by default; added / updated / deleted credentials take effect without restarting.
- **Auto-disable on authorization failure**: on 401/403 / `invalid token` / expired login, scheduling is suspended, the credential file is deleted, and a disabled marker is written; the account recovers automatically after a new `login`.
- **Background auto-renewal**: tokens are checked every 5 minutes; when expiration is within 15 minutes, they are refreshed automatically and written back to the credential file. Additionally, `-keepalive-hours` can refresh all accounts proactively at a fixed time (default 22:00 daily), preventing long-idle accounts from silently expiring.
- **Stream chunk normalization**: each upstream chunk's `finish_reason:""` is normalized to `null`, preventing the Anthropic translation layer from misjudging `stop_reason` and skipping tool execution.
- **Tool-call sequence self-healing**: before sending out, history structures with interleaved messages in parallel calls are repaired by `tool_call_id`, parallel calls split apart by the Responses API are merged, and unpaired calls, orphans, or duplicate results are removed, preventing the international site from returning `11148 tool_call_sequence_broken`.
- **OpenAI-compatible protocol**: `/v1/chat/completions` (SSE streaming + non-streaming aggregation), `/v1/responses` (Responses API), `/v1/models`, `/health`.
- **Native Anthropic protocol entry**: `/v1/messages` (SSE streaming + non-streaming aggregation) and `/v1/messages/count_tokens`; Anthropic clients such as Claude Code connect directly without an external translation layer, and auth is compatible with the `x-api-key` header.
- **Optional official outbound compatibility**: `upstream.requestProfile="official"` lets the three protocol entries share parameters, instruction structure, and outbound logic; domestic model requests use the collected static headers and gzip of a captured desktop build. Old configs default to `legacy`: no user-role promotion, no thinking-level overrides, and no tool replacement. See [outbound compatibility notes](outbound-compatibility.md) for details.

---

## Command Overview

```text
workbuddy-gateway [command] [options]

Commands:
  serve     Start the local gateway (default command; without a subcommand this is the same as serve)
  login     Log in and fetch / update credentials
  status    Show account pool status
  refresh   Manually refresh access tokens for all accounts
  monitor   Foreground live monitoring: account table + model stats table + recent logs
  probe     Actively probe whether accounts get a model for free or paid (requires serve to be running)
  reset     Clear all local data except login credentials, and re-fetch models and multipliers
  version   Show version info
  help      Show help
```

Global options (available for all commands):

| Option | Default | Description |
|---|---|---|
| `-addr <ip>` | `127.0.0.1` | Gateway listen address |
| `-port <port>` | `8317` | API listen port; when passed explicitly it overrides `config.json`'s `gateway.apiPort` |
| `-auth <path>` | Auto-discovery | Credential file path(s); supports a comma-separated list |
| `-auth-dir <dir>` | Empty | Credential directory; automatically loads all `workbuddy*.json` inside |
| `-proxy <url>` | Empty | Upstream request proxy, e.g. `http://127.0.0.1:7890`, `socks5://...` |
| `-verbose` | `false` | Output verbose debug logs |
| `-intl` | `false` | Only effective for `login`: log in to the international site |
| `-reload-interval <sec>` | `5` | Credential hot-reload scan interval; `0` disables |
| `-models-refresh <min>` | `60` | Model catalog refresh interval; `0` disables |
| `-allow-price-probes` | `false` | Allow background automatic price probing; sends real model generation requests that may consume credits. Off by default and does not affect normal request learning, catalog refresh, or the explicit `probe` command |
| `-webui` | `false` | Enable the standalone web console (default `http://127.0.0.1:8316/ui/`); the admin key and model API auth are configured in `config.json` |

**Price probing is off by default (including upgrades from old versions)**: background price probing only starts when `-allow-price-probes` or `-allow-price-probes=true` is passed explicitly. `-disable-price-probes` from old startup scripts still works; when both flags are passed and the old disable flag is `true`, disabling wins regardless of order. Passing only `-disable-price-probes=false` does not enable probing. This switch is a startup flag; changing it requires a service restart.

```bash
# Default: no proactive price probe requests; normal client requests still learn billing status automatically
workbuddy-gateway serve -webui

# Explicitly allow proactive probing; may cause extra credit consumption
workbuddy-gateway serve -webui -allow-price-probes
```

### Configuring the Web Console and Model API Auth

The `gateway` section of the working directory's `config.json` controls listen ports, the web console admin key, and model API auth. All values are stored in plaintext (`config.json` is Git-ignored; restrict its file permissions yourself):

```json
{
  "gateway": {
    "apiPort": 8317,
    "webPort": 8316,
    "adminKey": "",
    "apiKeyEnabled": false,
    "apiKey": ""
  }
}
```

| Config | Default | Description |
|---|---|---|
| `apiPort` | `8317` | Model API listen port; omitted or `0` uses the default; can also be temporarily overridden with `-port` |
| `webPort` | `8316` | Web console listen port; omitted or `0` uses the default; only listens when `-webui` is passed explicitly |
| `adminKey` | `""` | Web console admin key, settable two ways: **① One-click generation on the page** — when empty, open the page on the server itself (or via SSH local port forwarding); a random 32-character key is generated, written to this file, and shown **only once** in a dialog. **② Edit this file manually** — set the field directly to any random string (32 characters recommended); it takes effect hot in about 1 second after saving. Once set, other devices can log in with this key |
| `apiKeyEnabled` | `false` | Whether the model API validates a key. When off, calls work with no key or any key; when on, the key must match `apiKey` |
| `apiKey` | `""` | Model API key. Must be non-empty when validation is on; when off, the existing value is preserved and reused once you re-enable |

The web console can also toggle `apiKeyEnabled` and set `apiKey` on the "Access Settings" page; saving takes effect immediately and is written back to this file. `gateway.apiPort` / `gateway.webPort` also support hot switching: the new port is validated and bound first, then the old listener closes; existing connections and in-flight streams are not interrupted. The admin key and model API key are **independent**: disabling model API auth does not disable admin auth, and the model API key cannot log into the web console. To prevent someone else generating the key first, **one-click generation** of the admin key only works from the local machine; **manually editing `config.json` to set `gateway.adminKey`** has no such restriction and is exactly equivalent to one-click generation.

> **Upgrade order (important)**: the new version can read old configs without a `gateway` section, but **old binaries fail to start on new configs containing a `gateway` section** (`json: unknown field "gateway"`). **Replace the binary first, then add the `gateway` config.**

### JSON Debug Logs

`config.json` in the working directory controls structured debug logging, off by default. Changes take effect hot in about 1 second, no restart needed:

```json
{
  "debug": {
    "enabled": true,
    "fullRequests": false
  }
}
```

`config.example.json` and the default config generated on first deployment explicitly write `enabled: false` and `fullRequests: false` as placeholders.
A missing `debug` section, a `debug` that is `null`/empty object/empty string, or missing/empty values are all treated as `false`,
and never block startup; `0/1` and `"true"/"false"` are also parsed tolerantly, and unknown debug subfields are ignored.

When enabled, the gateway writes single-line JSON to `logs/debug-YYYY-MM-DD.jsonl`; normal run logs still go to the original `logs/gateway-YYYY-MM-DD.log` — neither replaces the other. Copy `config.example.json` as a starting point.

### Log Retention and Cleanup

```json
{
  "logs": {
    "retentionDays": 7
  }
}
```

- `retentionDays` ranges from `0–36500`: `0` or omitted keeps everything, compatible with old configs; `7` keeps today and the previous 6 days by the server's local date. **Once set to a positive number, expired logs are deleted automatically, and deletion is irreversible.**
- New rules apply after config hot reload; a scan runs at startup/on rule change, then automatically every hour. The web "Logs" page can save the retention days and also trigger cleanup under the saved rules immediately.
- Only `gateway-*.log`, `debug-*.jsonl`, `deployment-*.jsonl`, and `frontend-*.log` files with valid dates inside `logs/` are deleted; symlinks are not followed, and directories, credentials, configs, reports, or other files are never deleted, nor logs currently being written.
- Normal and debug logs switch to the new date's file on the first write after midnight. Old logs not yet switched remain protected and are cleaned up in later scans after retention is enabled.
- Replace the binary first, then add the `logs` section; old binaries may refuse to start over the unknown config field.

### Configuring the System Prompt

The `systemPrompt` section of the working directory's `config.json` has two independent options (hot-effective in about 1 second after changes):

```json
{
  "systemPrompt": {
    "fallback": "",
    "force": ""
  }
}
```

| Config | Behavior when empty | Behavior when non-empty |
|---|---|---|
| `fallback` | Continue using `You are a helpful assistant.` | **Only when the client sends no system content**, replaces the gateway's built-in fallback prompt; existing system content is unchanged |
| `force` | Disabled; the request prompt is processed entirely by the normal rules | **Experimental feature**: **appends** the configured text (post-positioned) to the **end of the first system content** in the Chat, Responses, and Anthropic Messages entries, making it the last, closest-to-user-message instruction; the original system content is fully preserved |

For example `{"systemPrompt":{"fallback":"You are an assistant.","force":"Answer in Chinese."}}`: when the client has no system, what goes upstream is `You are an assistant.\n\nAnswer in Chinese.`; when the client brings its own system, what goes upstream is `<client's original system>\n\nAnswer in Chinese.`. Config values only support JSON strings; empty or all-whitespace strings count as unset. Client user, assistant, and tool messages are never written out or replaced, and account selection is unaffected. Normal logs and normal JSON debug mode only record the rule branch, position, and character counts; with `debug.fullRequests` explicitly on, full request events include the prompt bodies before and after processing.

**Display identity replacement** (hot-effective):

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
      "Grok released by xAI": "WorkBuddy",
      "workspace root": "workspace base",
      "are paths the user": "are paths that the user",
      "You are an AI agent powered by DeepSeek Harness": "You are WorkBuddy",
      "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.": "You are interacting with the user through a Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.",
      "You are Claude Code, Anthropic's official CLI for Claude.": "You are Claude Code, Anthropic's official CLI tool for Claude.",
      "Main branch (you will usually use this for PRs)": "Default branch (you will usually use this for PRs)",
      "You are(.*)released by xAI": "You are WorkBuddy"
    }
  }
}
```

- When `identity` is omitted, known Harness names are replaced with `WorkBuddy` by default, plus the two equivalent compatibility replacements `workspace root → workspace base` and `are paths the user → are paths that the user`.
- Grok Build and other model tags are handled per the configuration in `identity.replacements`; the default leading wildcard handles `You are … released by xAI` rather than guessing identity from client detection code. Model selection and the request `model` stay unchanged.
- For different rewrite results, edit the full entries or the restricted leading wildcard rule below. Missing default entries are added at startup, and hot reload uses the configured mapping. Real upstream acceptance has not been verified.
- `target` can be `Assistant` or `WorkBuddy`; use `WorkBuddy` to stay closer to the official wording.
- `enabled:false` turns off replacement.
- For word-by-word control, add `replacements`, e.g.:
  ```json
  {"identity":{"enabled":true,"target":"WorkBuddy","replacements":{"Codex":"WorkBuddy","DeepSeek Harness":"WorkBuddy"}}}
  ```
- Only system/developer/assistant text is processed; user, tool, reasoning, and tool arguments are untouched. The official version bypasses the gateway, so its text is never rewritten.

#### Configuring Text Replacement and the Restricted Leading Wildcard

The current complete default config is in `config.example.json`, with **15 rules total: 14 plain literal rules and 1 restricted leading wildcard**. The old DSH identity long sentence, the GUI's known full two-sentence opening, the Claude CLI template, and the Main-branch template are all ordinary editable entries now; the matching engine no longer recognizes specific clients or hard-codes these sentences. First-time users get all rules generated; upgrades fill in missing entries at startup, and existing custom values, wildcard mappings, and the enable switch are preserved — no other config is overwritten.

Plain entries are still case-sensitive, matched globally as literals against the original text, including system/assistant bodies and code examples within them; the old DSH-specific case-insensitive matching, offset-zero checks, and GUI code-region detection no longer exist as special bypasses. To allow only leading matches, use the explicit wildcard rule below. User messages and tool data are always excluded.

This version generates the following entry by default and auto-fills it for upgraded users. You can also adjust it in your existing `identity.replacements`; do not overwrite your full config or keys with a fragment:

```json
{
  "You are(.*)released by xAI": "You are WorkBuddy"
}
```

For example, `You are Gemini 2.5 Pro released by xAI. Keep tools.` becomes `You are WorkBuddy. Keep tools.`. The trailing period, newlines, and body after the match are preserved verbatim. This rule does not judge client origin or sentence semantics: once enabled by default in this version, any text matching the pattern in allowed roles may hit, including some normal task instructions. To better distinguish a period from a domain suffix, you can set the fixed suffix to `released by xAI.` and keep the period in the replacement value accordingly.

Safety boundaries:

- Only the exact `(.*)` has wildcard meaning; every other character is treated literally. Full regex, capture references, and `$1` expansion are not supported.
- At most one `(.*)` per key; the fixed text before and after must each contain non-whitespace characters, and the key itself cannot span lines. Bare `(.*)`, `(.*)suffix`, `prefix(.*)`, and multiple wildcards are ignored with a warning, leaving other valid rules unaffected.
- Matching starts only from the first character of each standard text block; whitespace is not skipped automatically, and no middle-of-text or second-line searching. Prefix and suffix are case-sensitive.
- The dynamic part cannot span CR/LF and is capped at 256 Unicode characters; matching ends at the first fixed suffix, without greedily swallowing later paragraphs. When explicit prefix and suffix cover the entire short text, that text is still replaced — a consequence the rule requires.
- Rules execute from longest to shortest fixed text, with same-length literal rules first; at most one wildcard rule may hit per text block per round. Plain entries may still process text produced by earlier rules.
- Hot reload is supported after saving the config; invalid rules are logged with counts and reasons, and rule or prompt bodies are never written to normal logs.

Omitting or emptying `replacements` still uses the default rules as originally designed; to disable text replacement entirely, use `identity.enabled:false`. Deleting an entry while running removes the rule after hot reload, but startup migration still restores missing built-in entries; to disable a built-in rule across restarts, map it to itself (including wildcard keys — the matched original text is preserved and the literal `(.*)` is never inserted), and check whether other shorter entries could also match the same text.

`systemPrompt.fallback/force` remain independent explicit config features; `developer → system`, filling the fallback when system is missing, and merging consecutive systems in official mode are protocol-structure compatibility behaviors — not entry replacement — and are not turned off by the identity switch.

> **Experimental feature, use at your own risk:** `force` is appended to the end of system (right next to the user message), making it "more recent" than the client's own instructions and usually more influential on conflict — exactly the effect persona- and style-type instructions want, but it may also conflict with the client's system instructions, change model behavior, increase token usage, or trigger upstream content/parameter validation. To roll back, set `force` back to `""` and save; validate in a test session first, and never put passwords, keys, or personal privacy into the config. `config.json` is Git-ignored; do not commit your real config to the repository.

`/v1/messages/count_tokens` performs only a local approximation over client-provided content; it does not call upstream and does not count the `fallback` / `force` text the gateway injects afterward; final consumption is governed by the usage the model returns.

### Plan Recognition and Official-Site Consistency

Since v1.13.9, an account plan is no longer judged "Pro trial" merely from `ProTrialStatus=1`. Each quota refresh also calls the official site's read-only general resource endpoint, confirming the plan from active status, entitlement start/end times, and current subscription info:

- Queries are not limited to `PackageCodes` and support pagination; new plan codes from the official site will not be missed by a fixed list.
- Names are per-site: the four domestic tiers display canonically as **Experience / Standard / Advanced / Flagship**; the international site keeps **Free / pro / Pro trial**. The domestic Standard tier is no longer mislabeled as the international pro.
- New plans preferentially show the `PackageName` returned by the API, and historical plans are never forced into the four tiers. New codes without names show "Unrecognized subscription" instead of being guessed as Free or Pro.
- Bonus credit packs and top-up packs are not treated as subscriptions; a plan whose quota is exhausted but whose entitlement has not expired keeps its plan identity.
- Expired, refunded, or not-yet-active entitlements are not treated as the current plan. The trial flag is still `1`, but when there is no active trial and only Free entitlements, "Free" is shown.
- When the query fails or hits unexplainable fields/sub-products, an existing verified result is marked `*` (pending refresh), and an unverified result shows "Unconfirmed". Quota queries still update normally on success; quota is not dropped because plan recognition failed.
- Snapshots gain `planCheckedAt` and `planStale` fields. Unverified plan labels from old versions are not carried forward as trusted results; the new version re-verifies after startup.

Compatibility boundary: with the same product and resource field structures, newly added plans usually display the official name directly; if the official site changes product codes, APIs, status enums, or entitlement rules, a program update may still be needed. This is not a guarantee against any future API change. This process only queries membership/quota info — it never claims trials, purchases/renewals, or requests models.

### Enterprise Member Accounts

When `account.enterpriseId` in the credentials is non-empty, quota queries use the read-only endpoint
`POST /v2/billing/meter/get-enterprise-user-usage`, and an empty personal resource pack is no longer misjudged as enterprise quota exhaustion.

- `limitNum` is the quota assigned to this member and `credit` is what has been used this period; remaining is `max(0, limitNum - credit)`, keeping decimals. Compatible with `limit_num`, `used_num`, `usedNum`.
- `limitNum = -1` means unlimited; total and remaining quota display "Unlimited". It is not a zero balance nor an unknown query.
- Showing "Enterprise" only indicates the account type; it does not infer personal plan level, enterprise admin status, or the enterprise pool's total quota. Member quota and enterprise pool balance are two different things.
- Enterprise accounts skip personal checkins; token renewal and quota scans continue. Once enterprise quota recovers, model quota blocks are cleared automatically, but account/model rate-limit cooldowns are preserved; if a real model request returns `14018`, it is still blocked — "unlimited" cannot bypass an upstream rejection.
- On enterprise API timeout, failure, missing quota/used fields, field conflicts, or invalid values, the last quota is kept and marked pending refresh; failure is not treated as 0, and it does not auto-switch back to the personal endpoint.
- These branches are covered by local upstream mocks, integration tests across the three API entries, and web tests; environments without enterprise credentials cannot claim real enterprise account integration has been completed.

### Automatic Login Credential Renewal (Keepalive)

Login credentials involve two tokens: the **access token** is used to request models and query quota; the **refresh token** is used to obtain new access tokens. The refresh token itself also expires — once it does, or upstream rejects it, you must `login` again; the program cannot bypass this.

Since v1.13.11, the gateway renews through two trigger paths:

| Trigger | Timing | Description |
|---|---|---|
| Near-expiry renewal | background check every 5 minutes; refreshes when the access token has less than 15 minutes left | Ensures requests never use an expired token |
| **Proactive scheduled renewal** | local time given by `-keepalive-hours` (default `22`, i.e. 22:00 daily) | Refreshes all accounts before the access token nears expiry, preventing long-idle accounts from silently expiring |

```bash
# Default: proactive renewal daily at 22:00
workbuddy-gateway serve

# Daily at 10:00 and 22:00
workbuddy-gateway serve -keepalive-hours 10,22

# Disable proactive renewal; keep only on-demand refresh near expiry
workbuddy-gateway serve -keepalive-hours ""
```

Implementation notes:

- **Only calls the refresh endpoint**; no model requests, no quota consumption, no chat records.
- **Serial + throttled**: accounts are refreshed one by one with a 1-second gap to avoid the machine-like signature of bulk concurrent refreshes.
- **Startup catch-up**: accounts whose last successful renewal was over 20 hours ago get one catch-up round 90 seconds after service start, avoiding restarts that miss the scheduled time.
- **No hasty account deletion**: a single failure only increments a counter and keeps the original credentials; only 3 consecutive failures mark the login state invalid and stop scheduling. When upstream explicitly reports session invalidation (e.g. `12153` / `invalid_grant`), an alert fires on the first occurrence, but credentials are still kept until the threshold.
- **Renewal metadata is recorded**: after a successful refresh, `refreshExpiresAt` and `lastRefreshTime` are written back to the credential file; `status` and `monitor` show the refresh token's expiry and prompt re-login when it is within 7 days or already expired.
- **No problem masking**: if a refresh succeeds but the access token's lifetime is not extended, a separate alert notes the account may be approaching upstream's login-state limit.

> **Note that refresh tokens are exclusive.** Each renewal usually makes upstream issue a new refresh token and invalidate the old one. If the same account's credentials are used by other tools (scripts, other gateways, desktop clients) at the same time, both sides will invalidate each other's refresh tokens. Make sure one account is renewed from one place only. Leave `-keepalive-hours` empty to disable this feature.

#### Credential Overwrite and Pre-Deletion Verification

The credential file is the only copy of the login state; once deleted, you must log in again. So both irreversible actions — "overwrite" and "delete" — perform read-only verification first.

**After successful renewal, before overwriting the old credentials:**

| Check | Purpose | When it fails |
|---|---|---|
| Old and new credential account identities match (access token `sub`) | Prevents upstream account mix-ups or mismatched responses writing credentials to another account | Overwrite rejected; old credentials kept and an alert raised |
| Old and new credential sites match (`iss` realm) | Prevents domestic/international credentials overwriting each other | Same as above |
| New access token can fetch account data | Prevents a "returned 200 but actually unusable" credential from overwriting old credentials that still work | Same as above |

When the verification endpoint itself is unavailable, the check passes — a flaky verification will not reject valid new credentials. Verification failures do **not** count toward the "judged invalid" counter, so working accounts are never stopped because of it.

**After judging invalidation, before deleting credentials:**

1. Stop scheduling and write the disabled marker first;
2. Then use a read-only endpoint to confirm the credential really cannot fetch account data — **only then** delete the file;
3. If the credential can still fetch data → keep the file;
4. If it cannot be determined (network failure, upstream 5xx) → conservatively keep the file.

When the credential file is kept, the disabled marker remains effective across restarts and the account is not rescheduled. If you `login` again or manually renew that credential, the credential file becomes newer than the marker and the gateway automatically clears the stale marker and restores the account.

### Model Blocklist and Allowlist

The `models` section of `config.json` enables a per-model blocklist/allowlist (case and surrounding-whitespace insensitive):

```json
{
  "models": {
    "blocklist": ["deepseek-v4-pro"]
  }
}
```

- Text replacement uniformly uses `identity.replacements`, supporting any custom entries with hot reload. The duplicate `promptCompatibility` field once provided in v1.14.40 has been removed; old configs still containing it are read for compatibility and reported as ignored — its rules are not executed and startup is unaffected. Move any rules you want to keep into `identity.replacements`, then delete the old field after confirming.

| Field | Description |
|---|---|
| `blocklist` | Blocklist; any match disables the model |
| `allowlist` | Allowlist; **when non-empty**, only listed models are allowed and all others are disabled |

Rules:

- Choose **one** of a model blocklist or allowlist. A non-empty `allowlist` and a non-empty `blocklist` together fail validation to avoid config ambiguity; old examples with two empty arrays or one empty list still parse.
- When both lists are empty or omitted, no restriction applies (default behavior unchanged).
- Disabled models are **hidden outright** from `/v1/models`, the `model_count` in `/health`, and the model stats table in `monitor`.
- Requests for a disabled model return `403` with a message, **without consuming any upstream account quota**:

```json
{
  "error": {
    "message": "Model deepseek-v4-pro has been disabled by the gateway (matched the blocklist); contact the admin to adjust config.json",
    "type": "model_disabled",
    "code": 403
  }
}
```

The `serve` startup banner prints the current list status, e.g. `Model allow/block list: enabled (blocked 1 / allowed 0...)`.

### Per-Model Account File Restrictions

`models.accounts` is a blocklist/allowlist over credential JSON files: the reserved key **`all`** controls global account scheduling for all models, and other keys control the corresponding model. Without `all`, or with an empty global list, behavior matches old versions. Both the global rule and the model-specific rule must **pass** for scheduling to be allowed; a model-specific allowlist cannot bypass a global disable. This does not replace `models.allowlist` / `models.blocklist` above (which control whether a model is callable at all):

```json
{
  "models": {
    "blocklist": [],
    "accounts": {
      "all": {
        "allowlist": [],
        "blocklist": ["paused-account.json"]
      },
      "deepseek-v4.1-flash": {
        "allowlist": ["intl-a.json", "intl-b.json"],
        "blocklist": ["intl-b.json"]
      },
      "hy3": {
        "blocklist": ["old-account.json"]
      }
    }
  }
}
```

- Within one model, the blocklist wins; an empty account allowlist means no restriction, and an empty blocklist excludes nothing. In the example above, `deepseek-v4.1-flash` ultimately allows only `intl-a.json`, and `hy3` merely excludes `old-account.json`.
- Accounts in `all.blocklist` no longer participate in request scheduling, failure failover, or model generation probing for any model. Accounts still appear in the console; credentials, quota ledgers, and historical stats are retained. Background credential keepalive/quota queries are not model generation and keep their original behavior.
- When the global account allowlist is non-empty, the account pool toggle reflects the allowlist rule. The first web account operation deletes `all.allowlist` (including empty arrays or `null`), converts the **current account pool's** global enable/disable state into `all.blocklist`, then applies the operation; other current account states and model-specific rules are preserved, and accounts added afterwards default to enabled (blocklist semantics).
- Only credential **file names** (e.g. `intl-a.json`) are accepted — not paths or wildcards; when same-named credentials exist in multiple directories, matching is refused to avoid mistakes. Model names are case-insensitive; file names must exactly match the actual credential files.
- Neither request scheduling nor failure failover bypasses the account lists; background price probes and local `/admin/probe` also skip disallowed accounts. If no matching account exists, the request returns `403 model_account_disabled` with a message and upstream is never called.
- The live run log `logs/gateway-YYYY-MM-DD.log` records one summary line when accounts are excluded (model, candidate count, exclusion details); with debug on, `logs/debug-YYYY-MM-DD.jsonl` records `model_account_policy_checked` per account. The "Available accounts" column of the `monitor` model stats table is already filtered by the lists, counting only rule-compliant accounts.
- Changes to `config.json` take effect hot in about 1 second; file names in the example are placeholders. Without `models.accounts`, behavior is unchanged.

### config.json Hot Reload and Upgrades from Old Versions

`serve` independently checks config changes by file content every 1 second, unaffected by `-reload-interval 0` (which only disables the credential scan). Web toggles write the config file first, then apply immediately; edits from external editors apply on the next scan.

| Config | How it applies |
|---|---|
| Model lists, `models.accounts` (including `all`) | Hot-effective; controls subsequent scheduling |
| `gateway.adminKey` / `apiKeyEnabled` / `apiKey` | Hot-effective; after the admin key changes, log in again with the new key |
| `concurrent.MaxAcc` / `MaxGlobal` | Hot-effective; new requests queue immediately under the new concurrency limits, and in-flight requests are not interrupted |
| `systemPrompt` / `identity` / `debug.enabled` / `debug.fullRequests` | Hot-effective; affects only subsequent requests |
| `logs.retentionDays` | Hot-effective; the next cleanup run uses the new retention immediately |
| `upstream` timeouts, network retry counts | Subsequent upstream requests use the new values; in-flight streams keep the original idle timeout |
| `gateway.apiPort` / `webPort` | **Immediate hot switch** of listeners; the old listener closes only after all new ports bind successfully, and existing connections and streams finish normally |

- The config is parsed and validated as a whole before being applied. When runtime encounters half-written JSON, empty files, a briefly missing file, wrong types, or invalid account paths, it **keeps the previous valid config** and logs the reason to `logs/gateway-YYYY-MM-DD.log`. It retries automatically after you fix and save; a config read failure is never misread as "everything enabled".
- When a new port is occupied or the debug log file cannot be opened, the entire hot update is rejected; the original listener, auth, and debug switches stay as they were. A failed web save attempts to restore the old config file; a failed direct file edit keeps the original run state and waits for the file to be fixed. The two existing API/Web ports cannot be swapped directly — borrow a third free port first.
- Hot reload covers the fields `config.json` supports, not command-line startup flags (like `-addr`, `-webui`, `-reload-interval`, `-models-refresh`, `-keepalive-hours`). An explicit `-port` still takes priority over `gateway.apiPort`; after changing the web port, reopen the console on the new port.
- To restore default rules, save a valid `{}` or set the relevant lists to `[]` — do not delete the config file while running.
- Configs from the previous version that omit `all`, `accounts`, `models`, or `gateway` still parse; `accounts: {}`, `accounts: null`, `all: {}`, and empty lists all mean no global restriction. The default example includes empty `models.accounts.all.allowlist/blocklist`.
- On first startup without `config.json`, the program generates the complete default config from the built-in example. If an old config lacks the `identity` section, startup fills in the complete default section; if an existing `identity` lacks `replacements`, the complete default mapping is added, and existing custom mappings get each missing default replacement filled in individually — including `workspace root → workspace base` and `are paths the user → are paths that the user` — with user keys and values preserved. An existing `enabled:false` is never changed to on. When `target` is empty or `replacements` is empty, the default WorkBuddy rules run and startup is not blocked. User-defined `replacements` keys are not limited to built-in words.
- If an old config has **both kinds of non-empty model lists**, pick and keep one before upgrading; the new version no longer accepts conflicting model block/allowlists. A bad config while running keeps the last effective state, and startup reports a clear error.
- When an old config only had an allowlist, the console toggle reflects the allowlist; the first web model operation deletes `models.allowlist`, converting the **current known models'** enable/disable states into `models.blocklist`. Other known model states are unchanged, and models added afterwards default to enabled (blocklist semantics). Model account rules stay independent.
- In the new version, `all` is a reserved key; do not treat it as a real model ID. **Upgrade the binary first, then add `all`**; old binaries do not understand the global semantics and would treat it as a model-specific rule for a model named `all`.

Each JSON debug log includes time, level, stable event name, `trace_id`, `request_id`, service/instance/version, route, method, model, account, streaming flag, and cumulative duration, plus request metadata like client address, proxy headers, protocol, TLS, Content-Type, Content-Length, and deadline. A client-supplied `X-Trace-ID` is reused with priority and forwarded to upstream.

Request bodies are only recorded as these safe summaries, never as full bodies:

- `declared_body_bytes`, `actual_body_bytes`, `body_read_ms`
- `body_sha256_prefix` (first 12 hex chars of the SHA-256)
- `json_valid`, `json_decode_ms`
- `body_limit_bytes`, `body_limit_exceeded` (no request body limit is currently set, so these are `0` and `false`)
- `read_error_type`, and the sanitized, truncated `read_error`

Normal debug mode (`debug.fullRequests` absent or `false`) records neither full request bodies nor plaintext `Authorization`, Cookie, API Key, Access Token, or Refresh Token.

#### Full Request Comparison Logs (explicit opt-in)

When troubleshooting differences between clients or protocol translations, merge the following settings into your existing config; do not overwrite the whole config with this fragment:

```json
{
  "debug": {
    "enabled": true,
    "fullRequests": true
  }
}
```

Both switches must be on to take effect; hot reload takes about 1 second, no restart needed. Turning `fullRequests` off restores normal debug logs;
turning `enabled` off disables all JSON debug logs. As a production feature there are no temporary 30-minute or 200-request collection limits.

Records still go to `logs/debug-YYYY-MM-DD.jsonl`, chained by `trace_id`:

| Event | Content |
|---|---|
| `full_request_inbound_headers` | Model API inbound method, URL, and headers; the body has not been read at this point |
| `full_request_inbound` | The complete client JSON request body read after auth passes, under `body` |
| `full_request_upstream` | The complete upstream JSON after protocol translation, system prompt rules, and history repair; includes the actual outbound request headers |
| `full_request_upstream_result` | Per send: HTTP status, response headers, whether request headers/body were written out, and network error type |
| `full_request_upstream_error` | The upstream error JSON already read, e.g. `11128`; successful response streams are not pre-read |

Account failover within the same request is distinguished by `acc`, network retries by `attempt`,
and each actual send is correlated by `upstream_request_id`. The top-level `user_agent` field is always the client UA;
the actual outbound UA is in `full_request_upstream.request.headers` — do not confuse the two.

The full JSON is **never truncated**, including all messages, tool definitions, and image data; JSON whitespace/escapes may be re-serialized,
so it is not a byte-for-byte raw capture. Credential fields, cookies, known secrets, and credential parameters in URLs are redacted;
sensitive request header fields get an HMAC digest generated with a per-process random key, usable to judge equality only within the same process
and not comparable across restarts. Invalid or incompletely read JSON is explicitly marked with `body_capture` as omitted,
avoiding credential leakage from broken JSON while never pretending to be a complete capture.

Auth/CORS/method interception does not read the body extra just for logging; the related rejection reasons are still recorded by the existing middleware events.
Admin config, account login, and credential endpoints are outside full body capture.

**Privacy and disk note:** full requests contain real conversations, source code, tool inputs, and images — sensitive logs even with credentials redacted.
There is no guarantee every arbitrarily formatted secret inside the body is detected. Files are created with `0600` (on Windows also check the directory ACL);
do not upload or publicly share raw logs. Multi-megabyte requests grow disk usage and log write time quickly, so enable this only while troubleshooting;
the existing `logs.retentionDays` cleanup policy still applies. The web log viewer has a read size limit; for full comparison, read the file on the server.

---

## serve

Starts the local gateway; the default command.

```bash
# Default listens on 127.0.0.1:8317, auto-loads all workbuddy*.json in the current directory
workbuddy-gateway serve

# Custom port and listen address
workbuddy-gateway serve -port 9000 -addr 0.0.0.0

# Explicitly specify multiple credential files (comma-separated, round-robin)
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# Directory mode: load all workbuddy*.json in the directory
workbuddy-gateway serve -auth-dir ./auths

# Upstream via proxy + verbose logs (client auth is configured in the gateway section of config.json)
workbuddy-gateway serve -proxy http://127.0.0.1:7890 -verbose

# Disable credential hot reload
workbuddy-gateway serve -reload-interval 0

# Disable automatic model catalog refresh
workbuddy-gateway serve -models-refresh 0
```

Endpoints served after startup:

| Method | Path | Description |
|---|---|---|
| POST | `/v1/chat/completions`, `/chat/completions` | Chat Completions, SSE streaming and non-streaming |
| POST | `/v1/responses`, `/responses` | OpenAI Responses API |
| POST | `/v1/messages` | Anthropic Messages API (direct Claude Code connection; internally translated to Chat on the same upstream pipeline) |
| POST | `/v1/messages/count_tokens` | Anthropic token counting (local estimate; CJK 1 token/char, ASCII 4 chars/token) |
| GET | `/v1/models`, `/models` | Model list; the `X-Model-Source` response header marks the source |
| GET | `/health`, `/ping` | Health check returning `version`, `model_count`, `model_source` |
| POST | `/admin/probe` | Called by the `probe` command; **accepts loopback origins only** |
| GET | `/ui/` | Web console static page (separate port; exists only when `-webui` is enabled) |
| GET | `/admin/api/*` | Web console data APIs (separate port; exists only when `-webui` is enabled; requires the admin key) |
| GET | `/` | Simple text description |

Since v1.13.15, the Responses `function_call_output.output` supports text and image content block arrays:
text is preserved as-is in the `tool` result, and `input_image` images are promoted into a
`user` multimodal message placed after all paired tool results, with image URLs and data unchanged. Images are no longer serialized
as Base64 text into the context, preventing screenshot sessions from consuming tokens abnormally and eventually triggering `11133 model_param_invalid`.
Results of parallel tool calls remain in sequence and pairing is not broken by the inserted image. Plain string and JSON
tool results keep their original behavior; the gateway does not truncate operation manuals or session history. `11133` itself is a generic upstream parameter
error code, not limited to image or context-length issues; troubleshooting still needs the model, TraceID, and logs together.

Since v1.13.17, when the client declares no output budget (neither `max_tokens` nor `max_completion_tokens` carries a valid value), the gateway adds a `max_tokens` based on the output cap declared in the model catalog. Upstream judges window overflow by "input tokens + output budget", and without that field upstream reserves 384000 for output by default:
`deepseek-v4.1-flash` with a nominal 1048576 window would actually have only about 664576 input available, and long sessions get rejected with `11133 model_param_invalid` long before the window cap — appearing as "suddenly a 400 mid-chat". In testing,
678912 input tokens without a budget returned `400/11133`, while with `max_tokens=128000` it returned `200`. Values explicitly
passed by the client are never overridden; models missing from the catalog keep the original behavior and the gateway does not guess. Note that available input and the output cap
share the same window (available input = window − declared output budget); for longer input, have the client declare a smaller
output budget, not a larger one.

Background tasks (run after `serve` starts, per their enable conditions):

| Task | Cadence | Description |
|---|---|---|
| Token renewal check | 5 minutes | Auto-refresh when expiration is within 15 minutes |
| Quota scan | 5 minutes | 10-second timeout per credential; the old value is kept on timeout; `remaining=0` marks paid exhausted |
| Model catalog refresh | 60 minutes | Live API + npm catalog, merged, deduplicated, and written to cache |
| Model price probing | Off by default; when on, first round in ~20s, then every 2 or 30 minutes; at least 12 hours between same site+model probes | Only with `-allow-price-probes`; up to 30 per round without existing probe records, then at most 5; real generation requests may consume credits |
| Daily checkin | Random during normal `08:00–09:30`; after `08:00`, pending accounts catch up sequentially on startup with at least a 1-minute gap; failures retry only once during `09:30–10:00` | Domestic personal accounts only; hot reload events do not trigger makeup checkins directly |
| Status snapshot | 3 seconds | Writes `workbuddy-status.json` for `monitor` to read |
| Credential hot reload | 5 seconds | Scans for credential additions / updates / deletions |

**Upstream Timeout Policy**

The gateway no longer imposes a "total timeout for the entire stream" on upstream requests, avoiding long-running but steadily producing streams being cut off mid-flight (which loses usage, returns partial content, and gets flagged as a fault by upstream proxies, triggering 503):

| Stage | Policy |
|---|---|
| Connection and response headers | `ResponseHeaderTimeout` defaults to 300 seconds. Upstream queueing + preprocessing for 3MB+ requests can approach 1 minute (a 2.3MB request once took 50.8s to return headers), so the default is relaxed to 5 minutes as a fallback |
| Streaming response body | Idle read timeout defaults to 120 seconds: as long as data keeps arriving it never times out; only 120 seconds with no new data is it judged stuck and interrupted |
| Control short requests | Token refresh / quota queries / model catalog etc. use a 60-second timeout |
| Server response writing | No total duration cap (previously 300 seconds); long streams are never truncated by the server |

Both upstream timeouts can be overridden in the `upstream` section of the working directory's `config.json` (in seconds; omitted or non-positive values use defaults):

```json
{
  "upstream": {
    "requestProfile": "official",
    "headerTimeoutSeconds": 300,
    "idleTimeoutSeconds": 120,
    "transientRetries": 2,
    "streamKeepaliveSeconds": 15,
    "enableFirstByteRetry": true
  }
}
```

**Streaming Heartbeat and Pre-First-Byte Replay (v1.14.24)**

Tencent edge nodes may reset the SSE connection after the response headers were sent but before the first valid token arrives.
At that moment the client has received no model content yet. The Chat streaming entry provides two layers of mitigation:

| Item | Default | Description |
|---|---|---|
| SSE heartbeat | 15 seconds | Overridable via `upstream.streamKeepaliveSeconds`; omitted or non-positive values use the default. While waiting for the first valid data, `: keepalive` comments are sent, stopping after the first valid data |
| Pre-first-byte replay | On | Can be explicitly disabled with `upstream.enableFirstByteRetry: false`. Replays once through the account pool only when the client received no valid data, the error is a transient network fault or upstream idle timeout, and the replay budget is unused |
| Replay cap | 1 | No replay once any valid data was sent, on client cancellation, scanner errors, or non-network errors |

The heartbeat is a standard SSE comment and produces no fake tokens. A replay may consume one extra upstream credit call but never repeats already-generated content to the client. Replays and heartbeats are both logged with the existing TraceID; on replay failure the already-established SSE response is kept, ending with an in-stream error event and never faking `[DONE]`.

**Transient Network Error Retry**

A single TCP connection between the gateway and the upstream CDN edge may be reset by the peer (`connection reset by peer`), closed (`use of closed network connection`), or hit a recycled keep-alive connection. These are transient faults unrelated to request size (in testing, >5MB requests succeeded 95% of the time, while even 0.5MB requests occasionally failed).

Only when the **request headers have not been written out** can the gateway be sure upstream could not have processed the POST; only then does it retry transient connection errors. If `Do` returns EOF, RST, or a timeout but the request headers were written, upstream may have already received and processed the request; even without a response header yet, it will not auto-replay, to avoid duplicate generation.

| Item | Default | Description |
|---|---|---|
| Retry count | 2 | Overridable via `upstream.transientRetries`; set `0` explicitly to disable |
| Retry interval | 300ms | Gives the upstream edge time to recover |
| Retry scope | Transient errors when request headers were not written | Including connection-establishment EOF, RST, `broken pipe`, `connection refused`, etc. |
| No retry | — | EOF/RST/timeout after headers were written (outcome uncertain), client cancellation, upstream HTTP error responses |
| Connection handling | New connection | Retries set `Connection: close` and call `CloseIdleConnections()`, avoiding reuse of a bad connection |

Retries leave a clear record in the logs:

```text
[Network retry] traceId=... requestId=389 account=example.json retry 1/2, previous attempt's request headers were not written out, connection rebuilt
```

The startup banner prints the effective values for confirmation. When a stream is interrupted, no fake `[DONE]` (chat) or `response.completed` (Responses) is emitted; instead a clear `upstream_stream_interrupted` / `response.failed` error event is sent, so downstream never mistakes partial output for a complete result.

---

## login

Logs in and saves credentials. The domestic site prints a terminal ASCII QR code; the international site completes login in the browser.

```bash
# Domestic site (WeChat / WeCom QR scan)
workbuddy-gateway login

# Save to a specific file (recommended for multiple accounts)
workbuddy-gateway login -auth workbuddy2.json

# International site (in-browser; email / verification code / SSO)
workbuddy-gateway login -intl
workbuddy-gateway login -intl -auth workbuddy-intl.json
```

Notes:

- By default credentials are saved to `workbuddy.json`; `-auth` can specify another path.
- International credentials are written with `edition: "intl"` and can be mixed with domestic credentials in the same account pool.
- Logging in again overwrites the original credentials and automatically clears that account's disabled marker — no service restart needed (hot reload handles it).

---

## status

Shows account pool status, including site, cooldown, quota, and token expiry times.

```bash
workbuddy-gateway status
```

Example output:

```text
================== WorkBuddy Account Pool Status ==================
Total accounts: 2

--- Account #1 ---
Credential file: workbuddy.json
Site:            Domestic site (copilot.tencent.com)
Nickname:        user-a
User UID:        uid-xxx
Enterprise ID:   (personal account)
Auth domain:     www.codebuddy.cn
Cooldown:        Available
Token status:    Valid
Expires:         2026-09-22 12:32:07 (119h30m0s remaining)
```

---

## refresh

Immediately refreshes the Access Token of all accounts (normally the background checks every 5 minutes, so manual runs are unnecessary).

```bash
workbuddy-gateway refresh
```

- Success / failure / skipped (authorization invalid) are counted separately.
- If a refresh failure is authorization-related, the account is disabled and its credential file deleted.

---

## monitor

Foreground live monitoring, periodically refreshing the "account table + model stats table + recent logs" view; `Ctrl+C` to exit.

```bash
# Must run from serve's working directory (reads workbuddy-status.json)
cd /opt/workbuddy-gateway
workbuddy-gateway monitor

# Also show recent systemd service logs (Linux)
workbuddy-gateway monitor -journal workbuddy-gateway

# Also show a specific log file
workbuddy-gateway monitor -logfile /var/log/workbuddy-gateway.log

# Adjust the refresh interval and log line count
workbuddy-gateway monitor -interval 2 -lines 20
```

| Option | Default | Description |
|---|---|---|
| `-interval <sec>` | `3` | Status refresh interval |
| `-journal <svc>` | Empty | Also show recent `journalctl -u <svc>` logs |
| `-logfile <path>` | Empty | Also show the tail of the specified log file |
| `-lines <n>` | `15` | Number of log lines shown per refresh |

**Account table**

```text
Account pool: 2 total | Available 1 | Cooling 0 | Paid exhausted 1 | Expired 0 | Invalid 0
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| #    | Credential file| Account    | Site   | Status     | Token valid until   | Total    | Used     | Left   | Plan       | Free models| Model cooldown|
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
| 1    | workbuddy.json | user-a     | Domestic | Available | 2026-09-22 12:32:07 | 2300     | 1200     | 1100   | pro        | 2          | 0          |
| 2    | workbuddy2.json| user-b     | International | Paid exhausted | 2027-09-05 01:57:00 | 1100     | 1100     | 0      | Pro trial  | 2          | 0          |
+------+----------------+------------+--------+------------+---------------------+----------+----------+--------+------------+------------+------------+
```

Status values: `Available`, `Cooling`, `Paid exhausted`, `Expired`, `Invalid`.

The account pool table and CLI `monitor` no longer show the "Free models" column, replaced by a "Checkin" column; the account details in CLI `status` also show checkin. This change does not remove model multipliers, free-model detection, the measured ledger, or model scheduling logic.

### Daily Checkin: Random Plan and Same-Day Status

- All dates and execution windows are computed in **Beijing time UTC+8**, regardless of the server's local timezone. Each domestic personal account independently generates a random first plan (from `08:00`, before `09:30`) and a failure-retry plan (from `09:30`, before `10:00`) each day.
- After the first success or an explicit upstream "already checked in today" response, the status shows **Checked in** and no more requests are made that day; a first failure shows **Failed**, with exactly one more send at that account's retry time. A second failure ends the day — network errors, refresh state, credential hot reload, or restarts never add attempts.
- Before the scheduled time, or while waiting in the startup catch-up queue, the status shows **Pending**; brief in-execution states also count as pending. When no request was made and the account was already marked as having missed its window, the status shows **Missed** — "not executed" is never disguised as "checkin failed". International and enterprise accounts show `—`; when the state file is abnormal the status shows **Unknown** and checkins pause.
- Plans and attempt counts are saved in `checkin-state.json` in the run directory; the file contains only account hashes and fixed status info — no tokens, UIDs, nicknames, or credential bodies. **The attempt is claimed to disk before the request**, so an interrupted process does not resend the original request. Keep this file; do not treat it as a disposable cache. It is Git-ignored.
- **Started before 08:00 Beijing time**: only restores or generates random plans, no immediate checkin. **Started or restarted from 08:00 on**: only domestic personal accounts whose today status is still pending with 0 attempts enter the startup catch-up queue; with one account it runs immediately, with several the first runs immediately and each subsequent one runs at least 1 minute after the preceding request completes — no concurrent catch-ups.
- The startup catch-up is not limited by the random first window's `09:30` cutoff, but only handles today in Beijing time, never crossing days. Checked-in, failed, previously interrupted, or already-marked-missed accounts never enter the pending catch-up queue. If the first catch-up happens after `10:00` and fails, there are no more retries that day; the original `09:30–10:00` failure retry window was not widened.
- Queue times, used attempts, and the latest catch-up time are saved to the state file; repeated restarts never squeeze remaining accounts onto the current moment. The next request is never sent while the previous one is unfinished; failure retries within the same startup queue also respect the 1-minute minimum gap. Newly added accounts still follow the normal random plan and are not immediately caught up by hot reload events.
- If the process exits mid-request, the claimed attempt is not returned: an account whose first result was unconfirmed has only its original single retry left; if the second result is also unconfirmed, no more attempts that day. On state corruption, unwritable disk, or Beijing time moving backwards, conservative protection applies and no new checkins are sent, avoiding duplicates.
- After midnight Beijing time, yesterday's "checked in" no longer counts as today's success. The web hover checkin status shows the day's explanation and planned times.
- Next-day plans are generated after the Beijing-time day rollover; when all of today's tasks are done and nothing is pending, the next time is left empty — never faking a fixed "tomorrow 08:00 checkin".
- Checkin sends exactly one normal checkin request — no simulated chats or other activities; the account's model credential lock is not held during network waits. The random time is only a scheduling method; it does not represent platform authorization for automated activity, nor any guarantee against risk controls.

Plan values: `Pro trial` (upstream `ProTrialStatus=1`), `pro` (upstream `IsPaidUser=true`), `Free` (everything else, including unrecognized).

**Model stats table**

```text
Model stats (source live-api@2026-09-17 14:57):
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
| Model                      | Domestic rate    | Intl rate        | Avail accts | Requests | Avg TTFT(5h)  | Avg total(5h)   | Total tokens(M) |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
| hy3                        | 0.00x            | 0.00x            | 8        | 3        | 1.9s          | 2.3s            | 0.12M      |
| deepseek-v4.1-flash        | 0.03x            | 0.00x            | 5        | 12       | 820ms         | 3.4s            | 1.75M      |
| hy4-preview                | 0.00x            | Paid (unknown rate) | 8     | 4        | 1.3s          | 4.1s            | 0.00M      |
+----------------------------+------------------+------------------+----------+----------+---------------+-----------------+------------+
```

| Column | Meaning |
|---|---|
| Model | Model ID |
| Domestic rate | Effective domestic multiplier (`credits` × promo factor); free shows `0.00x`, `-` when the promo expired or the API has no valid rate, and `Paid (unknown rate)` when probing confirmed paid |
| Intl rate | Same as above for the international site |
| Available accounts | Number of accounts currently schedulable for this model (account cooldown, model cooldown, and model quota blocks all counted) |
| Requests | Number of client requests |
| Avg TTFT(5h) | Valid first-token latency in the last 5 hours (TTFT v2), counting only complete responses with real increments; bucketed hourly with automatic eviction of stale samples |
| Avg total(5h) | Average total duration over the last 5 hours' rolling window |
| Total tokens(M) | Cumulative upstream usage tokens since process start, in millions; prefers `usage.total_tokens`, falling back to `prompt_tokens + completion_tokens`; requests without upstream usage are not estimated |

---

## webui (Web Console)

Off by default. With explicit `-webui`, a standalone web console is served on `gateway.webPort` (default `8316`) for managing account pool toggles and model list toggles, viewing and cleaning logs, previewing and editing `config.json`, viewing scheduled task records, and toggling model API key validation or editing ports on the "Access Settings" page. The web UI does not show a credentials page. It is separate from the model API port; when not enabled, the admin port does not listen.

```bash
# Only an explicit -webui enables the web console; the model API still listens on 8317
workbuddy-gateway serve -webui

# Option 1: open the page locally or via SSH local port forwarding, generate the admin key with one click
ssh -L 8316:127.0.0.1:8316 root@server
# Open in a browser:
# http://127.0.0.1:8316/ui/

# Option 2: without opening the page, set the admin key manually on the server (32 random characters recommended)
python3 - <<'PY'
import json, secrets, pathlib
p = pathlib.Path("/opt/workbuddy-gateway/config.json")
cfg = json.loads(p.read_text())
cfg.setdefault("gateway", {})["adminKey"] = secrets.token_hex(16)  # 32 chars
p.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n")
print("gateway.adminKey written; the running new version hot-reloads it in about 1 second")
PY
systemctl restart workbuddy-gateway
```

| Item | Description |
|---|---|
| Entry | `GET /ui/` (standalone admin port; the static shell contains no data) |
| Data APIs | `GET /admin/api/status`, `/admin/api/models`, `/admin/api/logs`, `/admin/api/config`, `/admin/api/tasks` — **all require `Authorization: Bearer <adminKey>`** |
| Write APIs | `POST /admin/api/setup` (local-machine first-time admin key generation only), `POST /admin/api/settings` (model API auth and ports), `POST /admin/api/account-toggle` / `model-toggle` (account/model toggles), `POST /admin/api/credential-edit` (credential JSON), `POST /admin/api/log-settings` / `log-cleanup` (retention settings and expired cleanup) |
| Pages | Overview (usage trends, account pool health, and log alerts), Account Pool (global scheduling toggles), Model List (enable/disable toggles, including disabled models), Logs (retention days and cleanup), Config (key-redacted preview; after confirming the warning you can edit the full config, with the systemPrompt body shown directly), Scheduled Tasks (read-only log view), Access Settings (auth and port editing). The web UI no longer provides a credential list or editor |
| Data source | Reuses `serve`'s status snapshots, the full model catalog, `logs/` logs, and local config/credential files; toggles are written to the config under control and applied through the existing scheduling filters |
| Scheduled tasks | Cadence, enabled state, last run, completion time, result, next estimated run, and log notes; display only — tasks are never triggered or rescheduled |

#### Adding Accounts and Deleting Credentials on the Web

The Account Pool page has an "Add Account" button at the top. Clicking it:

1. Choose the **Domestic site** or **International site**;
2. Enter a **credential name**, defaulting to the first unused `workbuddyN.json` in the run directory. Only the file name is editable; the directory is fixed to the program's run directory (or the `-auth-dir` directory when set), and no absolute path is shown;
3. Click "Fetch login endpoint" and the form directly shows the **login link and QR code**, with a "Copy link" button beside it;
4. After you authorize via QR scan on your phone, the gateway writes the credentials, merges the account into the pool, and triggers a quota scan — **no service restart needed**.

This flow **reuses the same login implementation** as the CLI `workbuddy-gateway login -auth <file>` (plus `-intl` for the international site), so behavior is identical and the two cannot drift apart. Login sessions have a time limit (5 minutes domestic, 15 minutes international) and the page shows the remaining time; on timeout it reports the login link expired and stops polling, and clicking "Close" also stops polling. Neither case writes any file.

The last column of each account row has a "Delete" button: after confirmation, it deletes that account's credential file in the run directory and immediately removes it from the pool. **Only the credential file is deleted**; historical stats and logs are kept. Deletion is always refused when: the same-named credential exists multiple times, the file name does not match `workbuddy*.json`, the target is not a regular file (directory or symlink), or the account does not exist.

#### Read-Only Scheduled Tasks and Access Settings

- "Scheduled Tasks" aggregates 11 background tasks from the existing `[Scheduled task]` JSON records in `logs/gateway-*.log` via the read-only `GET /admin/api/tasks`, including the startup renewal catch-up. There is no new task scheduling module, task state file, or persistent registry; task events are still recorded with debug off.
- Each task event includes a task ID, round TraceID, process PID, start/completion times, result, and known next plan. Only records from the current process since startup are aggregated; old processes and corrupted lines never stand in for current successes; with no records, "No execution records" is shown. The log scan budget is 16 MiB, and reaching it is clearly reported.
- The three high-frequency tasks — config checks, credential scans, and status snapshots — record their latest execution every 60 seconds, with actual/state changes recorded immediately. The tasks page reads the logs every 15 seconds; periodic next times can be inferred from the scheduled anchors in the logs and are marked "estimated", since event triggers may come early. One-shot tasks are not rescheduled after completion; expired plans that cannot be inferred wait for new logs, never faking results.
- The web credentials page and editor have been removed; full credentials are no longer read or displayed automatically. The old `/admin/api/credentials` and `/admin/api/credential-edit` backend endpoints are temporarily kept for compatibility and still protected by independent admin auth, same-origin checks, file scoping, and version conflict protection, but the web UI no longer calls them.
- The web page title and branding use "Local Gateway", no longer showing the old brand name; protocol headers, config formats, and data fields stay compatible, and original text in user configs, logs, or account nicknames is never rewritten.
- "Account Pool"'s "Show available accounts only" keeps only accounts with `active` status whose global scheduling is not disabled; "Model List"'s "Show available models only" keeps only enabled models with more than 0 available accounts. Unchecking shows everything; the filters never change scheduling, config, or historical stats.
- The two filters are saved via cookies respectively, restored after refresh or re-login, and unchecked by default. The cookie contains only a `0/1` display preference, scoped to `/ui` on the current host, valid for one year, with `SameSite=Lax` and `Secure` over HTTPS; the cookie stores no keys or credentials and cannot be used to log in. With browser cookies disabled, filtering still works, but the page notes that a refresh may not remember the state.
- Background refresh updates tables partially by account/model key, preserving unchanged rows, toggle focus, and expanded details; charts are reused when stats are unchanged. Operation results appear in a bottom-right overlay that does not shift the page: success toasts close after about 5 seconds, error toasts are not cleared by unrelated polling, and they close only when the related endpoint recovers or a new operation toast replaces them.
- Next to the domestic / international rates on "Model List" there is a `!` button; clicking shows the rate's **data source**, **updated time**, and a one-sentence **plain-language explanation** (e.g. "catalog rate given by the official API", "the official promo ended at …, but the API still returns 0.00x — this value is not trustworthy", "last real probe verdict: paid (not retested yet)"); other sources get a sentence each under "Other records". Raw logs are collapsed by default under "Raw logs (N entries, collapsed by default)" and expanded only when needed, avoiding flooding.
- When a rate cannot be pinned to a value (e.g. "Paid (unknown rate)"), a **how to refresh** hint also appears: if background auto-probing is off, it says "request this model once with an account on that site, and the gateway will re-judge automatically from the returned `usage.credit` and write a log"; if probing is on, it says it will retest automatically. The toggle state in that hint is likewise parsed from normal logs (`[ModelPrice] stage=auto probe scheduling`), not an extra status.
- Details **never show wording like "Status: unknown"**: that verdict was actually measured, and writing "unknown" would suggest it was never tested; the actual meaning is conveyed by "last real probe verdict: paid (not retested yet)" and "how to refresh".
- When the logs contain no records for the model at all, it shows **no logs** with the note "request this model once with an account on that site and a new rate record will appear", without guessing a source.
- Rate sources only read `GET /admin/api/rate-detail` (requires the admin key, GET only), reusing the scheduled-tasks page's log reading: it scans and parses the tail of `logs/gateway-*.log`, adding no scheduler, state file, or in-memory ledger; results are cached for 20 seconds and fetched on click, not rescanned with every overview poll.
- Parsed log sources: `[Models] official multiplier` (recorded on first sight of or change to the official catalog), `[ModelPrice]` (probe verdicts, including cache loads), `[ModelCost]` (measurements from normal requests). Priority matches the page's value order: normal-request measured free > probe free > probe paid > promo expired > official value.
- Price-related logs all go to the **normal log** (`logs/gateway-YYYY-MM-DD.log`), independent of `debug.enabled`; these records exist even with debug logs off. Logs record only the model name, site, rate, credits, promo times, and account file name — never tokens.
- "Access Settings" can edit `gateway.apiPort` / `gateway.webPort`; saving calls the existing `POST /admin/api/settings`. Configured and actual listening ports are shown separately, and changes **hot switch immediately**; the old listener closes only after all new listeners bind, and existing connections and streaming requests are not interrupted. If the page itself is open on the old web port, reopen it on the new one; an explicit `-port` still takes priority over the config.
- The "Config" page shows a risk warning before "Edit Config"; after confirming, the real raw content (including keys) is loaded via `GET /admin/api/config-edit?acknowledged=1`. Use it only over trusted networks, HTTPS, or SSH forwarding. The editor never stores the body in browser storage; it clears the raw content when you leave the config page, exit editing, or the session expires, and unsaved changes require confirmation to discard.
- "Validate and Save" submits `content`, the SHA-256 `revision` read at load time, and `acknowledged: true` via `POST /admin/api/config-edit`. The config is capped at 1 MiB and must be a single JSON object conforming to the existing field and type rules; treating `[REDACTED]` as a key is rejected. If the config was changed by another web operation or the server since it was read, `409` is returned and the newer version is not overwritten; the web keeps your input for review and warns to discard unsaved changes before reloading.
- Saving the full config is equivalent to editing the server file directly: every field in `config.json` hot-applies, including listen ports; log retention rules may trigger irreversible deletion. After the admin key changes, old sessions are logged out and re-login is required; clearing the admin key needs extra confirmation with `confirmAdminKeyRemoval: true`, and afterwards it must be set again on the server itself. On config write, port binding, or hot-apply failure, the old file and old listeners are restored on a best-effort basis; logs record only stage, result, and length — never the body or keys.
- The "Logs" page reads and writes `logs.retentionDays` via `GET/POST /admin/api/log-settings`, and cleans expired logs via `POST /admin/api/log-cleanup` (empty JSON object), returning deleted count, reclaimed bytes, skipped in-write count, and failure count.
- All new endpoints require the independent admin key, and reuse same-origin checks plus TraceID audit logging; the model API key cannot edit credentials, change ports, or clean logs.

- When `-webui` is not enabled, the admin port does not listen and `/ui` with `/admin/api/*` routes are not registered.
- **Two ways to set the admin key** (either one):
  1. **One-click generation on the page**: when `adminKey` is empty, open `http://127.0.0.1:8316/ui/` on the server itself, or via SSH local port forwarding (`ssh -L 8316:127.0.0.1:8316 user@server`), and click to generate a random 32-character key; it is written to `config.json` and **shown only once** in the dialog. Remote (non-local) access cannot trigger one-click generation, preventing someone else from generating it first; but remote login with an already-set key still works.
   2. **Edit the config file manually**: edit `config.json` directly on the server, set `gateway.adminKey` to your own random string (32 characters recommended); it takes effect in about 1 second after saving. This way needs neither local access nor the page; just enter the string at login.
- `/admin/api/logs` only allows reading normal, debug, deployment, and frontend audit logs with restricted names under `logs/`, rejecting arbitrary paths and directory traversal; returned logs are first sanitized for secret values, and each read has a scan budget of about 1 MiB with overly long single lines omitted.
- The frontend is native HTML/CSS/JS embedded in the binary via `go:embed`, with no extra build step or runtime dependency; the admin key is stored only in the current tab's `sessionStorage`, saved only after successful verification.
- When the status snapshot is missing or corrupted, the APIs return `503`; the page keeps the last data with a notice, never displaying a read failure as zero accounts.

---

## Upstream Session Request Headers (v1.14.19)

Following the conversation/turn/message identifier semantics of the official WorkBuddy 5.8.4 client, the gateway fills in
`X-Conversation-ID`, `X-Conversation-Request-ID`, and `X-Conversation-Message-ID` for every model call.
International requests with tool declarations previously could trigger `11128 conversation id missing`; this compatibility layer neither disables tools or thinking,
nor changes prompts, output budgets, or the existing auth fingerprint.

- All three protocol entries (Chat Completions, Responses, Anthropic Messages), streaming and non-streaming aggregation,
  and both domestic and international sites go through the same outbound path; manual `probe` and background price probing share the same fallback.
- The client's `X-Conversation-ID` is read first, then `X-Session-ID`, then
  `conversation_id`/`conversationId`/`session_id`/`sessionId` from the request top level or `metadata`.
  Valid UUIDs are kept; other valid session keys are isolated per caller and turned into stable UUIDv5. Overly long keys or keys with control characters are ignored.
- `X-Conversation-Request-ID` and `X-Conversation-Message-ID` may be provided by the client,
  or via same-named underscore/camelCase fields at the top level or in `metadata`; they are generated independently when missing.
  Newly generated message IDs use the client's main-path 32-character hyphen-less UUID format.
- Safe network retries and cross-account/site fallbacks within the same logical HTTP request reuse all three identifiers.
  A new HTTP request regenerates turn and message identifiers by default. **Without an explicit session key, each HTTP call gets an independent session**;
  session ownership is never guessed from prompts, usernames, or model names; for stable sessions across turns, provide the session key explicitly.
- Only headers are generated or normalized; original session keys, request bodies, or tokens are never logged; these headers are not added to login, renewal, or quota endpoints.

## probe

Free/paid attributes are learned per "account (including site) + model", and the ledger only records accounts that actually requested that model. Default scheduling prefers accounts with balance, so **exhausted accounts are almost never selected and never learn attributes**. `probe` exists to fill that gap proactively.

> Note: quota-exhausted accounts are rejected wholesale by upstream (`14018 Credits exhausted`), so even free models fail. To verify whether a model is free, use an account **whose quota is not exhausted**.

```bash
# Probe all accounts, taking the first 5 catalog models per account
workbuddy-gateway probe

# Probe only specified accounts
workbuddy-gateway probe -auth workbuddy4.json

# Specify models
workbuddy-gateway probe -auth workbuddy4.json -models hy3,deepseek-v4.1-flash

# Cap the number (default 5, max 50)
workbuddy-gateway probe -auth workbuddy4.json -limit 8
```

| Option | Default | Description |
|---|---|---|
| `-auth <path>` | All accounts | Probe only the given credentials (file name or path both work) |
| `-models <m1,m2>` | First few in catalog | Models to probe |
| `-limit <n>` | `5` | Number of models probed when `-models` is not given, max 50 |

Example output:

```text
Requesting http://127.0.0.1:8317/admin/probe (account=workbuddy4.json, models=hy3)...

account                 site   model    result   credit  tokens  note
workbuddy4.json         intl   hy3      paid     0.42    820     usage.credit=0.42, paid

Summary: paid=1
```

Result statuses:

| Status | Meaning |
|---|---|
| `free` | `usage.credit=0` and `total_tokens ≥ 100`; learned as free |
| `paid` | `usage.credit > 0`; learned as paid |
| `unknown` | No `credit` returned, or `credit=0` but the sample is too small |
| `quota` | `14018` quota exhausted; recorded as paid for that account+model and the model blocked |
| `rate_limited` | `6004` model-level rate limit; only that model cools down |
| `auth_failed` | Authorization invalid (probe does not auto-disable accounts) |
| `skipped` | Account invalid or no credentials |
| `error` | Network / protocol error |

> How it works: `probe` acts as a client calling the running service's `/admin/probe`. The ledger lives in the `serve` process's memory; a separate process writing the state file directly would be overwritten by the service snapshot, so probing must be done by the running service. This endpoint accepts loopback origins only; when model API key validation is on, `gateway.apiKey` must be carried as well.

---

## reset

Clears **all local data except login credentials** and re-fetches models and multipliers.

```bash
workbuddy-gateway reset
```

Cleanup scope:

- `workbuddy-status.json` (account and model status snapshot)
- `wb-models-cache.json` (model catalog, multipliers, price probe verdicts)
- `*.disabled` / `*.json.disabled` (authorization-invalid markers)
- `logs/` (run logs)

Kept: `workbuddy*.json` login credentials.

After cleanup, the model catalog and multipliers are re-fetched immediately. The account ledger also lives in the `serve` process's memory; if the service is running, restart it so everything resets in sync:

```bash
systemctl restart workbuddy-gateway
```

---

## version / help

```bash
workbuddy-gateway version    # Prints WorkBuddy Local Gateway vX.Y.Z
workbuddy-gateway help       # Prints full help
workbuddy-gateway -v         # Same as version
workbuddy-gateway -h         # Same as help
```

---

## Multi-Account Pool

Three configuration methods:

```bash
# Method 1 (recommended): auto-discovery
# Put multiple credential files in the working directory; no flags needed
workbuddy-gateway serve

# Method 2: comma-separated -auth
workbuddy-gateway serve -auth workbuddy.json,workbuddy2.json

# Method 3: -auth-dir directory
workbuddy-gateway serve -auth-dir ./auths
```

Behavior notes:

- **Round-robin**: requests are dispatched round-robin across available accounts.
- **429 cooldown**: `6004` only cools down the triggering model; only 429s that cannot be attributed to a model enter account-level cooldown, which recovers automatically when it expires.
- **Authorization invalid**: 401/403-class errors disable the account and delete its credential file, writing a `*.disabled` marker; it recovers automatically after a new `login`.
- **Quota exhausted**: `remaining=0` marks "paid exhausted"; the account can still serve models confirmed free.
- **Hot reload**: scans every 5 seconds by default; credential additions / updates / deletions need no restart.
- **Concurrency control**: by default each credential account handles at most 4 concurrent model requests, with no global limit. All model APIs share the limits below; credential maintenance still uses exclusive protection, so tokens are never refreshed while model concurrency is open.

### Per-Account and Global Concurrency

```json
{
  "concurrent": {
    "MaxAcc": 4,
    "MaxGlobal": 0
  }
}
```

| Config | Default | Description |
|---|---|---|
| `MaxAcc` | `4` | Concurrent model requests per credential account; positive integers take effect; 0, negatives, missing, or empty fall back to 4 |
| `MaxGlobal` | `0` | Cap on model requests across all accounts combined; positive integers take effect; 0, negatives, missing, or empty mean unlimited |

- Without a config file, first startup generates a default file including this section. The first startup after an upgrade fills in this section and any missing fields in old files; existing values, account rules, keys, and identity config are not overwritten.
- When `concurrent` is `null`, an empty string, an empty object, or a field is `null`, an empty string, a non-integer/wrong type, each field runs at its default without affecting other valid config. Unknown field names are still rejected by config validation, so typos cannot silently fail.
- After editing the file, the existing config watcher hot-reloads automatically (checked every second); saving the full JSON from the web also applies immediately. No restart needed.
- Scaling up wakes queued requests; scaling down never force-cancels in-flight requests — admission resumes once usage naturally drops to the new cap. Hot reload never rebuilds counters, so no extra slots appear and none get mis-released.
- A request must satisfy both the per-account and global caps to be admitted; while waiting on a busy account, no global slot is held. Once a client cancels a queued request, nothing more is sent upstream.
- A concurrency slot is held from admission until the upstream response body is fully read, closed, or cancelled — **including the entire streaming response**, not just the "send to response headers" phase. Safe retries, failovers, and abnormal endings all release their slots.
- This limit covers the shared upstream calls of the three model APIs — Chat Completions, Responses, and Messages; background quota, checkins, and credential maintenance do not count toward model concurrency.
- No queue-timeout config was added: waiting requests wait for a slot or client cancellation; upstream timeouts continue using the `upstream` section. Actual sustainable concurrency depends on the model, context length, accounts, and upstream load; raising the cap does not guarantee no 429s.

---

## Model List and Multipliers

**List sources**: the live API `GET {Base}/v2/enterprises/personal/models` and the static npm package catalog, deduplicated by model ID with the **API taking priority**.

```text
Both succeed   → merged and deduplicated
One succeeds   → the successful one is used
Both fail      → the local cache wb-models-cache.json is used
Fail, no cache → that site shows no models this round (model calls unaffected)
```

**Free-site priority**: if a model is "free on one site, paid on the other", scheduling prefers accounts on the free site until all of that site's accounts are unavailable (cooling / exhausted / invalid), then falls back to the other site; if both sites are free or both paid (only multipliers differ), no priority is set and normal round-robin continues.

**Multipliers**:

```text
1. Promo active:      effective rate = credits × factor (factor=0 → 0.00x)
2. Promo expired:     API credits are untrustworthy (upstream often bakes the promo price into credits);
                      shown as - until a probe result arrives, then decided by measurement
3. Model not in the API catalog: also decided by measurement
4. No promo and credits present: that rate is shown directly
```

**Price probing (off by default)**: only passing `-allow-price-probes` explicitly enables background probing, which prefers same-site accounts with balance and may also use accounts with unknown balance. It sends real generation requests (`max_tokens=300`) that may consume credits; rate/batch limits are not a credit budget — there is currently no cumulative credit-spend cap.

The default-off only affects background proactive probing: catalog refresh still fetches official prices and promo times; normal client requests still learn paid/free from `usage.credit` (free is only recognized at zero credits and at least 100 total tokens). The explicit `probe` command can still measure proactively, and likewise may consume credits.

```text
Probed free  → shows 0.00x; while still a candidate, retested at least 12 hours apart
Probed paid  → shows Paid (unknown rate); may be probed again while still a candidate
14018 / no usage.credit / sample too small → not overwritten, stays unknown
```

Probe scheduling (effective only after explicit opt-in):

| Trigger | Description |
|---|---|
| Service startup | First round about 20 seconds after start |
| First run / after reset | Up to 30 per round, quickly filling in verdicts |
| After convergence | Up to 5 per round, at most once per model per 12 hours |
| Pending probes not cleared | Catch up on a 2-minute short interval; back to 30 minutes once cleared |
| Catalog refresh succeeds | Triggers a round immediately |
| Credentials change | Triggers a round immediately (including "a site had no account, then one was added") |

Only models actually requested, or explicitly flagged for confirmation by the API, are probed, avoiding pointless quota spend.

The `/v1/models` response header `X-Model-Source` and `/health`'s `model_source` mark the catalog source.

### Model Capability Extension Fields

Each model in `/v1/models` carries capability fields when the catalog has trustworthy data; **unknowns are always omitted, with no fallback like 1M**, preventing downstream from reserving context against a wrong window:

| Field | Meaning |
|---|---|
| `context_length` | **Model capability cap**, from upstream `maxInputTokens` (falls back to `maxAllowedSize` when missing). Determines how much input upstream accepts. |
| `max_output_tokens` | Output cap, from upstream `maxOutputTokens`. |
| `default_context_length` | **Client default budget**, from upstream `contextWindow.defaultLength`. It only decides where the client starts compressing, and is **not** the capability cap. |
| `context_length_options` | Client-selectable tiers, from upstream `contextWindow.supportedLengths` (given only with ≥2 tiers, ascending). |
| `capabilities_by_site` | Given only when the domestic and international sites declare different values for the same model, listing the four fields above per site. |

Key points:

- `context_length` (capability cap) and `default_context_length` (default budget) have different semantics and must be read separately. Take `deepseek-v4.1-flash`: cap 1000000, default budget 300000, tiers 300000 / 600000 / 1000000.
- Same-named models may be served by domestic or international accounts with different capabilities. **When they disagree, the max is never faked**: top-level scalars are left empty and per-site details go into `capabilities_by_site`, so downstream is never routed to a smaller-window account while reserving against the larger value.
- These fields describe capability only — **they neither inject upstream request parameters, nor truncate messages, nor change the existing output budget (`max_tokens`) logic**. How much context the client actually sends is still decided by the client's own compression strategy.
- During catalog refresh, the log prints a `[Model capabilities]` coverage summary (per-site counts of models with a cap / tiers / output cap), making it easy to check which models lack data.

---

## Usage and Performance Statistics (stats.json)

The gateway maintains an optional `stats.json` in the **program working directory**, recording usage facts grouped by credential for the web console's "Overview" page.

### What Is Recorded

| Content | Description |
|---|---|
| Per-account model ledger | Each credential's `ModelStates` (per-model cost, quota blocks, cooldowns, last observed time), keyed from "credential file + site + UID + enterprise ID" |
| Per-call account invocations | Completion time, model, HTTP status, whether complete, input/output/total tokens, cache reads, `usage.credit`, total duration, first-token duration |

**Not recorded**: access tokens, refresh tokens, request or response bodies, prompts, account nicknames.

### Counting Rules

- **Counted per actual upstream account call**: one request triggering multiple account calls (failed retries, failover on quota exhaustion) records one entry per account called. Requests rejected before any account was selected (auth failure, bad parameters, disabled model) count toward no account.
- **Credits come only from `usage.credit`**, never inferred from "account balance deltas", so concurrent requests and quota refreshes are never counted as consumption.
- **Missing usage is recorded as an empty sample, not zero consumption**: the API returns `—` and coverage numbers like "Token 1/2 requests have samples", keeping 0 and unknown distinguishable.
- **Cache reads are already included in input tokens**, shown separately but never double-added.
- **First-token latency (TTFT v2)**: measured from the gateway's request entry until parsing the first non-empty text, reasoning, or valid tool-call increment ends; heartbeats, role declarations, empty content, usage, finish, and DONE are excluded. Tool names or non-empty argument increments count; a bare tool ID does not. Streaming/non-streaming across all three protocols, normal logs, CLI, and web all use the same measurement; truncated or cancelled requests never enter the TTFT average.
- **Completeness verdict**: decided only by the upstream stream itself — read errors or a missing end reason both fail; after a confirmed complete end, the client disconnecting upon receiving `[DONE]` is normal behavior, and that first-token sample is no longer discarded because the request context was later observed cancelled (fixed in v1.14.9).
- **Boundary with dsh**: the same valid-increment criteria are used, but the gateway starts at receiving the HTTP request (dsh's `step/start` is unavailable), so client-side lead time cannot be counted; the gateway still aggregates all calls for the chosen time/account/model, including auxiliary requests — not a dsh single-session average. For non-streaming, the first token is measured from upstream generation, not when the client receives the full JSON.
- **Backward compatibility**: new records carry `ttftMetric: "first_token_v2"`. Old records' first-response-body durations are kept but excluded from the new TTFT average; tokens, credits, and historical total durations still aggregate normally, and the web shows the count of old-scope samples excluded. New records' total duration is also timed from the gateway request entry, avoiding the inconsistency where first token appears larger than total duration after queueing.
- Time dimensions follow the **UTC+8 calendar**: hours aligned on the hour, **weeks starting Monday**, months by calendar month. In the request-duration chart, bars are first token and the line is total duration, with one bar and one line per model averaging only that model's own calls — never merging different models into one. When a model has no calls in a time bucket, the bar is skipped and the line crosses the empty bucket connecting adjacent samples — **never faking 0ms**; hovering shows the per-model readings for that bucket. The average total duration and average first token in the summary cards above are still averages over all calls in the current range.
- **Only requests after this feature was enabled are counted**; historical logs are not backfilled and no data is fabricated.

### Web Overview Page

The top of the "Overview" page is **run status** (account pool health and account log alerts), followed by "Usage & Performance": optional **granularity** (hour/day/week/month), **scope** (global or a single credential), and start/end dates; it shows five summary cards — total tokens, total credits, account calls, average total duration, average first token — two combined charts (usage trend: token line + credit bars with dual left/right axes; request-duration trend: total-duration line + first-token bars, both split per model), and a **model usage ranking** table.

The ranking table and charts share the same time range and account filter: only models with **actual calls** in the selected range are listed, sorted by total tokens descending (by request count when tokens are unknown), with the heaviest usage first; models without calls do not appear. The table also gives request count, complete/failed, input/output, cache reads, credits, average duration, average first token, and data coverage counts.

Charts are in-page native SVG with no external chart library loaded. The usage chart puts tokens (line, left axis) and credits (bars, right axis) in one chart with separate left/right scales since their magnitudes differ; time buckets that have started but have no token samples are connected at 0, making idle periods visible. Buckets that have not started are judged by the server time returned by the stats API, and both charts draw no points, lines, or bars and no 0-fill; the full aligned time axis is kept, with hover showing "not yet reached" and four `—` values. The currently in-progress bucket renders normally; when the old API has no server time, browser time is the fallback. In the duration chart, bars are first token and the line is total duration; the legend and ranking table use the same model set, following the current time and account range, and models without calls are not drawn. Both charts share plot margins and date ticks; hovering either chart runs one dashed line through both charts and the card gap, with a unified overlay showing that bucket's total tokens, credits, average total duration, and average first token; average duration uses the summary over the current time/account range, while per-model curves keep each model's own samples. Idle buckets have 0 usage, and when requests exist but usage is missing the overlay shows unknown; the duration chart connects its line across empty buckets but never disguises missing samples as 0ms. Interaction start and end are logged with the same TraceID; mouse coordinates, metric values, or credentials are never recorded, and nothing is reported per pixel.

Chart hover covers the entire dual-chart area, including plot areas, axis text, titles, card margins, and the gap between the charts; moving in the gap picks the nearer chart by distance for the overlay, with the dashed line and four readings following the same time position instead of stopping at the last plot area entered. The linked tooltip hides only after leaving the whole dual-chart area. Chart hover uses a `requestAnimationFrame` queue shared by both charts: only the last mouse position per frame is processed; within one time bucket the overlay content is not rebuilt, its size is not re-read, and hit points are not re-updated. The overlay and through-line move via `transform`; each frame reads both charts' geometry before updating the display, and the inactive chart does not measure the hidden overlay. Pending draw events are cancelled on leaving the chart, cancelling, switching pages, logging out, or redrawing, so stale overlays never reappear. The interaction audit still logs only start and end, not every mouse move.

### Overview Run Status: Health and Account Log Alerts

The run status area no longer repeats account-count and per-site-count cards; it is now health on the left and log alerts on the right; the Account Pool page keeps the detailed statuses. This block moved to the very top of the overview: check health and alerts first, then usage and performance.

- **Health = available accounts not globally disabled / all accounts in the pool × 100%**. Manually disabled accounts remain in the denominator; all available shows `100%`, and partial availability rounds down to one decimal. With no accounts or a stale snapshot, `—` is shown instead of faking full health.
- Health describes account-level status, not a per-model callability rate; a single model's cooldown is not the same as the whole account failing.
- Alerts come from `logs/gateway-YYYY-MM-DD.log`, compatible with the existing cooldown/quota-exhausted/invalid/disabled logs, plus `[Account status]` structured records. Recorded only at startup or on state change, including the TraceID, and never containing credentials, tokens, or upstream error bodies.
- Only records that are **still abnormal in the current snapshot and have a matching log** are shown, deduplicated per account per category; historical anomalies that recovered stop showing. When a log cannot be found, the missing entry is noted — never fabricated.
- Alerts sort by **log time descending, newest on top**; severity only affects color and label, not position. Equal times sort stably by account file name then alert category.
- What auto-clears after recovery is the matching anomaly category, not all of an account's alerts. For example, if quota recovers but the account is still manually disabled, only "Credits exhausted" is removed while "Account disabled" remains. This is not push-based: snapshot and overview each refresh about every 3 seconds; quota recovery also needs the background quota query to notice it first (one round every 5 minutes by default). The log scan's 10-second cache does not stop the latest snapshot from removing recovered items; on network or read failure, old results are kept with a notice.
- Severity: red "Critical" = account invalid; orange "Important" = account expired, credits exhausted; yellow "Attention" = account cooling; blue-gray "Notice" = manually disabled. Colors are paired with text labels.
- The log read budget is 4 MiB total, scan results are short-cached 10 seconds, and the list is capped at 100 entries. Reaching the range cap or missing logs produces a notice; on log read failure the old list is kept and marked failed, without affecting health queries or model calls.
- `/admin/api/account-alerts` is read-only and requires the admin key; account state cannot be changed through it.

### Robustness

- When `stats.json` **does not exist, statistics start silently from empty**, and persistence is attempted automatically after the first data.
- When **unwritable** (insufficient permissions, full disk, path occupied), it degrades to "memory only": statistics and display continue, the web shows "Memory only · not persisted", and the background retries automatically every 5 seconds — **never blocking or interrupting model requests**.
- When the **file is corrupted or not a regular file** (e.g. replaced by a symlink or directory), **the original file is kept, not overwritten**, likewise degrading to memory-only, so user data is never clobbered.
- Persistence uses "temp file in the same directory → `fsync` → atomic rename", with `0600` permissions; on a stop signal, the process flushes once first.
- The file has a safety size cap; once exceeded, writing stops and in-memory statistics are kept.

> This file contains account identifiers and usage data; it is `.gitignore`d — do not commit or publicly share it.

---

## Client Integration

After the gateway starts, the service address is `http://127.0.0.1:8317/v1`.

curl:

```bash
curl -N -s http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"hy4-preview","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

Python OpenAI SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8317/v1", api_key="none")
resp = client.chat.completions.create(
    model="hy4-preview",
    messages=[{"role": "user", "content": "write a quicksort"}],
)
print(resp.choices[0].message.content)
```

Claude Code (direct Anthropic protocol connection, no external translation layer):

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_API_KEY=your-model-api-key   # Any value works when apiKeyEnabled is off in config.json
claude
```

> The gateway performs bidirectional Anthropic → Chat translation at the `/v1/messages` entry: reasoning is translated back into `thinking` content blocks,
> `tool_calls` into `tool_use`, and `finish_reason` maps to `stop_reason` (`tool_calls→tool_use`,
> `length→max_tokens`), while reusing account pool round-robin, tool-sequence self-healing, and the WAF sanitization pipeline.
> For the model name, use any ID from the gateway's `/v1/models` list.

DSH (`~/.dsh/settings.yaml`):

```yaml
llm-pi-ai:
  providers:
    workbuddy-local:
      baseURL: http://127.0.0.1:8317/v1
      apiKeyEnv: LOCAL_API_KEY   # Any string works
      api: openai-completions
      models:
        - id: hy4-preview
          contextWindow: 1000000
          maxTokens: 128000
```

---

## Platform Deployment

### Windows

1. Download `workbuddy-gateway-windows-amd64.exe` from [Releases](https://github.com/CangShui/workbuddy-gateway/releases).
2. In PowerShell / CMD, go to the file's directory:

   ```powershell
   .\workbuddy-gateway-windows-amd64.exe login
   .\workbuddy-gateway-windows-amd64.exe serve -port 8317
   ```

3. Start at boot: `Win+R` → `shell:startup`, put a shortcut to the exe into the startup folder, and append `serve` after the shortcut's "Target".

### Linux

```bash
# x86_64
wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-amd64
sudo install -m 755 workbuddy-gateway-linux-amd64 /usr/local/bin/workbuddy-gateway

# ARM64
wget https://github.com/CangShui/workbuddy-gateway/releases/latest/download/workbuddy-gateway-linux-arm64
sudo install -m 755 workbuddy-gateway-linux-arm64 /usr/local/bin/workbuddy-gateway

workbuddy-gateway login
workbuddy-gateway serve -addr 127.0.0.1 -port 8317
```

#### systemd Service (recommended)

Create `/etc/systemd/system/workbuddy-gateway.service`:

```ini
[Unit]
Description=WorkBuddy Local Gateway (CodeBuddy OpenAI-compatible proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/workbuddy-gateway
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0 -port 8317
Restart=on-failure
RestartSec=5
User=root
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=false

[Install]
WantedBy=multi-user.target
```

Deploy and start:

```bash
sudo mkdir -p /opt/workbuddy-gateway
sudo cp workbuddy-gateway /opt/workbuddy-gateway/
sudo /opt/workbuddy-gateway/workbuddy-gateway login
sudo systemctl daemon-reload
sudo systemctl enable --now workbuddy-gateway
sudo systemctl status workbuddy-gateway
sudo journalctl -u workbuddy-gateway -f
```

> `WorkingDirectory` determines the auto-discovered credential directory. Put multiple credential files in it to form an account pool; additions / updates / deletions hot-reload automatically.

Common operations:

```bash
sudo systemctl restart workbuddy-gateway
sudo systemctl stop workbuddy-gateway
sudo systemctl disable workbuddy-gateway
```

When exposing externally (e.g. other LAN devices), change `-addr` to `0.0.0.0` and enable model API key validation in `config.json`:

```json
{"gateway":{"apiKeyEnabled":true,"apiKey":"sk-changeme"}}
```

```ini
ExecStart=/opt/workbuddy-gateway/workbuddy-gateway serve -addr 0.0.0.0
```

### macOS

1. Download `workbuddy-gateway-darwin-arm64` (Apple Silicon) or `workbuddy-gateway-darwin-amd64` (Intel).
2. Remove the quarantine attribute:

   ```bash
   chmod +x workbuddy-gateway-darwin-arm64
   xattr -d com.apple.quarantine workbuddy-gateway-darwin-arm64 2>/dev/null || true
   ```

3. Log in and start:

   ```bash
   ./workbuddy-gateway-darwin-arm64 login
   ./workbuddy-gateway-darwin-arm64 serve
   ```

4. Start at boot (launchd): create `~/Library/LaunchAgents/com.workbuddy.gateway.plist`:

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>com.workbuddy.gateway</string>
     <key>ProgramArguments</key>
     <array>
       <string>/path/to/workbuddy-gateway-darwin-arm64</string>
       <string>serve</string>
       <string>-port</string><string>8317</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>WorkingDirectory</key><string>/path/to/workbuddy-gateway-dir</string>
   </dict>
   </plist>
   ```

   ```bash
   launchctl load ~/Library/LaunchAgents/com.workbuddy.gateway.plist
   ```

---

## Security Notes

- `workbuddy*.json` files contain real access credentials (Access Token / Refresh Token) — **never commit them to Git or share them publicly**; this repo's `.gitignore` already excludes them.
- The gateway listens only on `127.0.0.1` by default. For LAN / public access, use `-addr 0.0.0.0` and enable model API key validation in `config.json` (`gateway.apiKeyEnabled`), or place it behind a reverse proxy.
- `gateway.adminKey` and `gateway.apiKey` in `config.json` are stored in plaintext as required — restrict the file's permissions; the web config preview hides keys, and real keys appear in the editor only after the admin explicitly confirms the edit warning. Do not share editor screenshots or edit on public devices; the full config lives only in the current page's memory and never enters audit logs.
- `/admin/probe` accepts loopback-origin calls only.
- When an account's authorization is no longer needed, delete its credential file and revoke the authorization in the CodeBuddy console.

---

## Building from Source

Requires the **Go 1.26.5 or newer toolchain** declared in `go.mod`:

```bash
git clone https://github.com/CangShui/workbuddy-gateway.git
cd workbuddy-gateway

go vet ./...
go test ./...

# Current platform
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o workbuddy-gateway .

# Cross-compile examples
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/workbuddy-gateway-linux-amd64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/workbuddy-gateway-windows-amd64.exe .
```

Standalone web and build script regressions are centralized in `tests/`:

```powershell
.\scripts\test.ps1
```

Go same-package tests uniformly use `snake_case_test.go` naming and stay in the source package directories, excluded from production builds. See [project-layout.md](project-layout.md) for the full directory guide and [tests/README.md](../tests/README.md) for test commands.

One-shot PowerShell build:

```powershell
.\build.ps1                         # Check and build all 9 targets
.\build.ps1 -Only windows           # Windows x86, x64, ARM64
.\build.ps1 -Only linux             # Linux x86, x64, ARMv7, ARM64
.\build.ps1 -Only linux -Architecture arm -ArmVersion 6
```

| OS | Architecture | Artifact |
|---|---|---|
| Windows | x86 32-bit / `386` (requires SSE2) | `workbuddy-gateway-windows-386.exe` |
| Windows | x86-64 / `amd64` | `workbuddy-gateway-windows-amd64.exe` |
| Windows | ARM64 | `workbuddy-gateway-windows-arm64.exe` |
| Linux | x86 32-bit / `386` (requires SSE2) | `workbuddy-gateway-linux-386` |
| Linux | x86-64 / `amd64` | `workbuddy-gateway-linux-amd64` |
| Linux | ARM 32-bit (defaults to ARMv7) | `workbuddy-gateway-linux-armv7` |
| Linux | ARM64 | `workbuddy-gateway-linux-arm64` |
| macOS | x86-64 / `amd64` | `workbuddy-gateway-darwin-amd64` |
| macOS | ARM64 | `workbuddy-gateway-darwin-arm64` |

- **Windows ARM32 is not supported by the current Go toolchain** and cannot be substituted with the ARM64 file. Linux ARM32 can choose `-ArmVersion 5/6/7`, with the file name matching the minimum ARM version.
- By default `go vet` and `go test` run first, unaffected by the caller's leftover `GOOS` / `GOARCH`. Every target uses `CGO_ENABLED=0`, with an x64 baseline of `GOAMD64=v1` and an ARM64 baseline of `GOARM64=v8.0`. The original build environment and working directory are restored after success or failure.
- Default output goes to `dist/`, also generating `SHA256SUMS`, `build-manifest.json`, and a `config.example.json` with empty keys; checksums cover the binaries, manifest, and example config.
- `-OutputDir` must be a dedicated subdirectory inside the project; `-Clean` only removes known build files and never recursively deletes directories, config credentials, or other files. Build logs go to `logs/build-YYYY-MM-DD.jsonl`.

---

## Disclaimer

This project is for personal learning and technical research only. Tencent CodeBuddy's (both the domestic site and international workbuddy.ai) interface protocols and risk-control policies may change at any time; comply with Tencent's terms of service and use at your own risk. This repository contains no officially undisclosed keys or credentials.
