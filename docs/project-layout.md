# Directory and Naming Conventions

This reorganization only changes file layout and test entry points; it does not change runtime working directories, config formats, gateway behavior, or binary build entry points.

## Directory Responsibilities

```text
workbuddy-gateway/
├── README.md                    # short entry point linking to full docs
├── *.go                         # production sources of package main
├── *_test.go                    # same-package Go unit tests, excluded from go build
├── go.mod / go.sum              # Go dependencies
├── build.ps1                    # keeps the original one-shot build entry point
├── config.example.json          # user config and Release packaging example
├── web/                         # only web runtime resources used by go:embed
│   ├── index.html
│   ├── app.js
│   └── style.css
├── docs/
│   ├── user-guide.md            # full user manual
│   ├── contributors.md          # contributor attribution
│   ├── project-layout.md        # this document
│   └── local/                  # local history reports, not committed
│       ├── reviews/
│       ├── deployments/
│       ├── releases/
│       ├── diagnostics/
│       └── issues/
├── tests/
│   ├── README.md
│   ├── build/build_script.test.ps1
│   └── web/app.test.cjs
├── scripts/test.ps1              # unified test entry point
├── archive/
│   ├── README.md
│   └── local/                   # local attachments unrelated to the gateway, not committed
├── dist/                        # local build output, not committed
│   └── local/                   # original root-directory binary kept during reorganization
├── logs/                        # runtime, build, and test audits, not committed
├── .codex-tmp/                  # pre-existing local isolated experiments, kept and ignored
└── .playwright-mcp/              # pre-existing browser acceptance cache, kept and ignored
```

`config.json`, account JSON, state files, and model caches are kept per the existing runtime conventions. The reorganization does not move runtime logs or credentials into doc directories, nor does it delete old artifacts.

## Why Go Tests Were Not Moved to tests/go

Current Go tests use `package main` and directly verify unexported functions, types, and global state. Go defines packages per directory, so moving these files into a subdirectory would create a different package that cannot access those private members.

So the standard Go same-package testing convention is kept:

- Production builds still use `go build .`, with Go automatically excluding `_test.go` files.
- Tests still run with `go test ./...`, with no overlay, custom loader, or duplicated test code.
- Test names follow `snake_case_test.go`, such as `runtime_config_test.go` and `model_account_test.go`.
- The browser acceptance fixture remains `web_policy_browser_test.go`, skipped by default and run only when its environment variable is set explicitly.

If the Go source and test directories are ever fully separated, first split the package along its public API, then rewrite the corresponding integration tests; never export all internals just for tidier directories.

## File Naming

| Type | Rule | Example |
|---|---|---|
| Go tests | `snake_case_test.go` | `price_probe_control_test.go` |
| Standalone script tests | `name.test.ext` | `app.test.cjs`, `build_script.test.ps1` |
| Public docs | lowercase `kebab-case.md` | `user-guide.md` |
| Dated local reports | `YYYY-MM-DD-topic-report.md` | `2026-10-06-pr-11-code-review-report.md` |
| Version history material | `v<version>-topic.md` | `v1.13.13-release-notes.md` |
| Build artifacts | keep the build script's OS/arch suffix | `workbuddy-gateway-linux-arm64` |

Production Go file names are unchanged for now to avoid needless source-path changes. When moving historical files, keep their content and do not rewrite the original experimental facts or log paths.

## Local Material and Release Boundaries

- `docs/local/`, `archive/local/`, tool caches, `dist/`, and `logs/` are all ignored and must not accidentally enter Git through this reorganization.
- The unrelated root-level `index.js` was archived as `archive/local/agent-loop-plugin.js`, no longer confused with the web entry; its content is unchanged.
- The root-level `workbuddy-gateway.exe` moved to `dist/local/workbuddy-gateway.exe`, without passing it off as a fresh build of the current version.
- One-off experiment scripts stay in their original cache directories to avoid breaking historical tool references; properly maintained development scripts go to `scripts/` going forward.
- Public docs and tests are not added to the Release runtime package; `config.example.json` stays in the root for the existing build scripts to package.

## Verification

Run from the project root:

```powershell
.\scripts\test.ps1
.\build.ps1 -Only linux -Architecture amd64 -OutputDir dist/layout-check
```

For test commands, web acceptance conditions, and log paths, see [tests/README.md](../tests/README.md).
