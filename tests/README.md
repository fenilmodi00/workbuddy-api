# Tests

Tests independent of the Go package are centralized in this directory; Go white-box unit tests remain alongside the production package. All commands run from the project root by default, and tests never use production accounts or model generation APIs.

## Unified Entry Point

```powershell
.\scripts\test.ps1                 # All: Go static checks/unit tests, web unit tests, simulated build regression
.\scripts\test.ps1 -Suite go
.\scripts\test.ps1 -Suite web
.\scripts\test.ps1 -Suite build
```

- Uses Go, Node.js, and PowerShell; missing tools required by the selected suite raise an error instead of being silently skipped.
- The Go test stage clears cross-compilation and browser-acceptance environments to avoid accidentally starting long-running fixtures; the caller's environment and working directory are restored on completion or failure.
- Simulated build tests do not run real cross-compilation and do not clean user directories.
- Runner audit: `logs/test-runner-YYYY-MM-DD.jsonl`, containing stages, results, and redacted output.

## Go

```bash
go vet ./...
go test ./...
go test -run '^TestConfigEditor' -count=1
go test -run 'TestOfficial|TestRequestProfile|TestHarnessProtocol' -count=1
```

`*_test.go` files use `package main` internals directly and are not moved into ordinary subdirectories. Go excludes them during `go build .`, and standard test commands still run the full suite with no extra loader needed.

Test processes use temporary working directories, and configs, credentials, and upstream responses are all synthetic data, avoiding pollution of real files.
If a cross-compiled test binary runs on another machine, provide the source directory of the same commit and specify its absolute path with `WORKBUDDY_TEST_SOURCE_ROOT`; that way example-config file tests locate files correctly even with `-trimpath`, so these tests need not be skipped.
Outbound compatibility tests cover the three protocols × streaming/aggregated, gzip, explicit-parameter preservation, hot reload, and request identity.
`identity_request_test.go` verifies that text replacement is controlled only by `identity` and stays effective across fresh installs/upgrades and hot reload; the legacy `promptCompatibility` field is read only for compatibility with a warning, executes no rules, does not affect user/tool content, and does not loosen validation of other unknown fields. Local HTTP tests verify three entry points × two outbound modes × streaming/aggregated × four literal/wildcard switch scenarios — 48 combinations — covering later system/assistant text blocks, model fields, and user/tool content protection.
`identity_wildcard_test.go` verifies a single `(.*)`, non-empty fixed prefix/suffix, zero offset, non-greedy matching, single-line/256-character cap, Unicode, no regex expansion, ignoring invalid rules without falling back to defaults, precedence, a mapping disabling itself, hot reload, and logs that never leak body text; it also verifies that legacy hardcoded rules run only when configured and enabled, ordinary templates no longer modify user/tool content, and that fresh installs/upgrades from older versions with the 15 default rules (including the default wildcard) keep custom values and disabled state and restart idempotently. Real upstream channel validation is not yet verified.
`stream_test.go` covers empty tool identity fields, argument fragments, extension-field protection, and interleaved tool chunks plus multi-segment reasoning text returned through Chat requests; `messages_test.go` verifies the thinking start event contains the string `signature` and preserves signature delta and end-event ordering. Tests use a local synthetic upstream; the scope of real-service validation is described in the outbound compatibility notes.
Optional offline replay of historical captures is described in the [outbound compatibility notes](../docs/outbound-compatibility.md#validation-and-boundaries); private material is not read by default.

## Web

```bash
node --check web/app.js
node --test tests/web/app.test.cjs
```

`tests/web/app.test.cjs` loads the real `web/app.js` with synthetic DOM / fetch responses, needs no browser dependency, and calls no production APIs. Audit logs go to `logs/webui-frontend-tests-dev-YYYY-MM-DD.jsonl`.

Optional real-browser regression requires Python Playwright and a usable Chromium and is not part of the default test entry:

```bash
python tests/web/polling.browser.py
python tests/web/polling.browser.py --browser "path to a local Chromium-compatible browser executable"
```

This test loads only the web files in the repository and intercepts all network requests with synthetic responses, verifying success/error notices, page navigation, account enable/disable, partial table updates, multiplier detail retention, and chart reuse; it never touches production or upstream, and acceptance records go to `logs/polling-flicker-*.jsonl`.

## Build Scripts

```powershell
.\tests\build\build_script.test.ps1
```

Uses mocked Go commands to verify the nine-target matrix, platform/arch filtering, known-file cleanup, failure handling, and environment restoration. Temporary material goes to `.codex-tmp/`, audits to `logs/build-tests-YYYY-MM-DD.jsonl`. This test cannot replace real cross-compilation.

## Manual Browser Acceptance

Default Go tests do not run the browser fixture. When explicitly enabled, it only starts the local `127.0.0.1:18316` admin page with synthetic accounts and an admin key, calling no upstream:

```powershell
$env:WORKBUDDY_BROWSER_FIXTURE = '1'
$env:WORKBUDDY_BROWSER_DURATION = '90s'
go test -run '^TestPolicyBrowserFixture$' -count=1 -timeout 3m -v
Remove-Item Env:WORKBUDDY_BROWSER_FIXTURE, Env:WORKBUDDY_BROWSER_DURATION
```

Keep the fixture duration below the test timeout. Never substitute production addresses, real keys, or real credentials into this fixture.

## Directory and Naming

See [docs/project-layout.md](../docs/project-layout.md). Do not add one-off experiments or reports that may contain environment info to the public test directories.
