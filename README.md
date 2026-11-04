# WorkBuddy Local Gateway

A cross-platform single-binary gateway in pure Go with zero CGO dependencies, supporting domestic and international sites, multi-account scheduling, the OpenAI / Anthropic protocols, and an optional web console.

## Quick Start

Requires the Go 1.26.5 or newer toolchain declared in `go.mod`. Run from the project root:

```bash
go build -o workbuddy-gateway .
./workbuddy-gateway login
./workbuddy-gateway serve -webui
```

Windows uses `.\workbuddy-gateway.exe`. The admin port defaults to `8316` and the model API to `8317`.

- On first start, a complete default `config.json` is generated in the working directory; older configs get missing `identity` and `concurrent` settings filled in, plus the default `workspace root → workspace base` and `are paths the user → are paths that the user` compatibility replacements; existing custom values are not overwritten. The default per-account model concurrency is `4` with no global limit; changing `concurrent.MaxAcc/MaxGlobal` hot-reloads.
- v1.14.43 moved the legacy DSH identity sentence, GUI description, and fixed template rewrites into `identity.replacements`, adding `"You are(.*)released by xAI": "You are WorkBuddy"` by default; fresh installs generate it, upgrades from older versions fill in missing entries on startup, and existing values plus `enabled:false` are preserved. There are 15 default rules in total; the leading wildcard is limited to a single line with at most 256 characters in its dynamic part, and client branding is not checked. Rules, switches, and target changes all hot-reload.
- **Background price probing is disabled by default** and only enabled by explicitly passing `-allow-price-probes`; proactively generated requests may consume credits.
- The web config page supports full JSON editing after a risk confirmation; API/Web listening port changes hot-swap, and existing connections and streams finish. The previous runtime state is kept when the new port is occupied or the config cannot be applied; an explicit `-port` still takes precedence over the config file.
- `config.json` and account credentials contain sensitive information, are ignored by Git, and should not be shared publicly.
- Multiple harnesses can enable unified outbound compatibility with `upstream.requestProfile: "official"`; older configs that omit this field keep the original behavior. See the [outbound compatibility notes](docs/outbound-compatibility.md).

## Documentation

| Content | Location |
|---|---|
| Full user manual, config, client integration, and deployment | [docs/user-guide.md](docs/user-guide.md) |
| Directory responsibilities and naming conventions | [docs/project-layout.md](docs/project-layout.md) |
| Contributors and solution attribution | [docs/contributors.md](docs/contributors.md) |
| Test entry points and notes | [tests/README.md](tests/README.md) |
| Multi-harness outbound alignment, protocol parameters, and compatibility boundaries | [docs/outbound-compatibility.md](docs/outbound-compatibility.md) |
| DSH history replay, GUI environment sentence, and 11128 compatibility | [docs/11128-dsh-history.md](docs/11128-dsh-history.md) |

## Build and Test

```bash
go vet ./...
go test ./...
```

PowerShell:

```powershell
.\scripts\test.ps1                 # Go, web, and build-script regression
.\build.ps1                       # Validate and build all 9 platform/arch targets
.\build.ps1 -Only linux -Architecture amd64
```

Go unit tests stay in the same directory as the `package main` source to preserve private-function tests and standard `go test ./...` behavior; they are excluded from production builds. Standalone web and build-script tests are centralized in `tests/`. Full documentation lives in `docs/`, with the build entry point and config example kept in the root.

This project is for personal study and technical research only; please comply with the upstream terms of service. Full security notes and the disclaimer are in the [user manual](docs/user-guide.md#security-notes).
