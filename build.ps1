<#
.SYNOPSIS
    WorkBuddy Local Gateway one-shot cross-compilation (pure Go, zero CGO).
.DESCRIPTION
    Builds 9 artifacts by default: Windows 386/amd64/arm64, Linux 386/amd64/arm/arm64,
    macOS amd64/arm64. Linux ARM32 defaults to GOARM=7 and the file name contains armv7.
    Windows ARM32 is not supported by the Go toolchain, so no fake artifacts are generated.
    Runs go vet + go test first by default; emits SHA256SUMS, build-manifest.json, config.example.json.
    Build audit is written to the project logs/; only known build files are cleaned, directories are never deleted recursively.
.PARAMETER Only
    Build only windows, linux, or darwin.
.PARAMETER Architecture
    Build only 386, amd64, arm, or arm64; the combination must have supported targets.
.PARAMETER ArmVersion
    Minimum ARM version for Linux ARM32: 5, 6, or 7, default 7.
.PARAMETER OutputDir
    Output directory, default dist; must be a standalone subdirectory inside the project directory, not a symlink.
.PARAMETER Clean
    Clean known build artifacts, manifests, and example config; keep other files and subdirectories.
.PARAMETER SkipTests
    Skip pre-build checks; only for repeat builds after checks have already passed.
.EXAMPLE
    .\build.ps1
    .\build.ps1 -Only linux -Architecture arm -ArmVersion 6
#>
param(
    [ValidateSet('', 'windows', 'linux', 'darwin')]
    [string]$Only = '',
    [ValidateSet('', '386', 'amd64', 'arm', 'arm64')]
    [string]$Architecture = '',
    [ValidateSet('5', '6', '7')]
    [string]$ArmVersion = '7',
    [string]$OutputDir = 'dist',
    [switch]$Clean,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot)
$trace = [Guid]::NewGuid().ToString('N')
$logDir = Join-Path $root 'logs'
[IO.Directory]::CreateDirectory($logDir) | Out-Null
$auditFile = Join-Path $logDir ("build-{0}.jsonl" -f (Get-Date -Format 'yyyy-MM-dd'))
$utf8 = [Text.UTF8Encoding]::new($false)
$environmentNames = @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOARM', 'GOAMD64', 'GO386', 'GOARM64')
$oldEnvironment = @{}
foreach ($key in $environmentNames) {
    $oldEnvironment[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
}

function Write-BuildAudit {
    param([string]$Event, [string]$Detail, [hashtable]$Fields = @{})
    $record = [ordered]@{
        timestamp = [DateTimeOffset]::Now.ToString('o')
        trace_id = $trace
        event = $Event
        detail = $Detail
    }
    foreach ($key in $Fields.Keys) { $record[$key] = $Fields[$key] }
    $json = Protect-BuildLog ($record | ConvertTo-Json -Compress -Depth 8)
    [IO.File]::AppendAllText($auditFile, ($json + "`n"), $utf8)
}

function Protect-BuildLog {
    param([string]$Text)
    $Text = $Text -replace 'github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9]+', '[REDACTED]'
    $Text = $Text -replace '(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+', '$1[REDACTED]'
    return $Text -replace '(https?://)[^/\s:@]+:[^@\s/]+@', '$1[REDACTED]@'
}

function Set-BuildEnvironment {
    param([string]$Name, [AllowNull()][string]$Value)
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}

function Invoke-Go {
    param([string[]]$Arguments, [string]$Phase)
    $PSNativeCommandUseErrorActionPreference = $false
    Write-BuildAudit 'go_started' 'Running Go tooling; never prints environment variables or credentials' @{ phase = $Phase; arguments = $Arguments }
    $output = @(& $goCommand @Arguments 2>&1)
    $code = $LASTEXITCODE
    foreach ($line in $output) { Write-Host (Protect-BuildLog ([string]$line)) }
    Write-BuildAudit 'go_completed' 'Go command completed' @{ phase = $Phase; exit_code = $code; output = ($output -join "`n") }
    if ($code -ne 0) { throw "Go phase $Phase failed, exit code $code; see $auditFile" }
    return ,$output
}

function Get-SafeOutputDirectory {
    $path = if ([IO.Path]::IsPathRooted($OutputDir)) {
        [IO.Path]::GetFullPath($OutputDir)
    } else {
        [IO.Path]::GetFullPath((Join-Path $root $OutputDir))
    }
    $comparison = if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        [StringComparison]::OrdinalIgnoreCase
    } else { [StringComparison]::Ordinal }
    $prefix = $root.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if (-not $path.StartsWith($prefix, $comparison)) {
        throw 'Output directory must be inside the project directory; project root or outside paths are not allowed'
    }
    $cursor = $path
    while ($cursor.Length -ge $root.Length) {
        if (Test-Path -LiteralPath $cursor) {
            $item = Get-Item -LiteralPath $cursor -Force
            if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "Output directory and its parents must be ordinary directories; files, links, or reparse points rejected: $cursor"
            }
        }
        if ($cursor.Equals($root, $comparison)) { break }
        $cursor = [IO.Path]::GetDirectoryName($cursor)
    }
    return $path
}

$failure = $null
Push-Location -LiteralPath $root
try {
    Write-BuildAudit 'build_requested' 'Build started; checks run by default, cleaning only touches restricted output files' @{
        only = $Only; architecture = $Architecture; arm_version = $ArmVersion
        output_directory = $OutputDir; clean = [bool]$Clean; skip_tests = [bool]$SkipTests
    }
    $goCommand = Get-Command go -ErrorAction Stop
    $versionMatch = Select-String -LiteralPath (Join-Path $root 'main.go') -Pattern 'version\s*=\s*"([^"]+)"' | Select-Object -First 1
    if (-not $versionMatch) { throw 'Unable to read version from main.go' }
    $version = $versionMatch.Matches[0].Groups[1].Value
    $minimumGo = ((Get-Content -LiteralPath (Join-Path $root 'go.mod') | Where-Object { $_ -match '^go\s+' }) -replace '^go\s+', '').Trim()
    foreach ($key in $environmentNames) { Set-BuildEnvironment $key $null }
    Set-BuildEnvironment 'CGO_ENABLED' '0'
    $toolchain = (Invoke-Go @('version') 'toolchain') -join "`n"
    $toolVersion = [regex]::Match($toolchain, 'go(\d+\.\d+(?:\.\d+)?)')
    if ($toolVersion.Success -and ([version]$toolVersion.Groups[1].Value -lt [version]$minimumGo)) {
        throw "Current Go version is too old; go.mod requires Go $minimumGo or a newer toolchain"
    }
    $supported = Invoke-Go @('tool', 'dist', 'list') 'supported_targets'
    $output = Get-SafeOutputDirectory

    $targets = @(
        @{ OS = 'windows'; Arch = '386'; Suffix = '386'; Ext = '.exe' }
        @{ OS = 'windows'; Arch = 'amd64'; Suffix = 'amd64'; Ext = '.exe' }
        @{ OS = 'windows'; Arch = 'arm64'; Suffix = 'arm64'; Ext = '.exe' }
        @{ OS = 'linux'; Arch = '386'; Suffix = '386'; Ext = '' }
        @{ OS = 'linux'; Arch = 'amd64'; Suffix = 'amd64'; Ext = '' }
        @{ OS = 'linux'; Arch = 'arm'; Suffix = "armv$ArmVersion"; Ext = '' }
        @{ OS = 'linux'; Arch = 'arm64'; Suffix = 'arm64'; Ext = '' }
        @{ OS = 'darwin'; Arch = 'amd64'; Suffix = 'amd64'; Ext = '' }
        @{ OS = 'darwin'; Arch = 'arm64'; Suffix = 'arm64'; Ext = '' }
    )
    if ($Only) { $targets = @($targets | Where-Object { $_.OS -eq $Only }) }
    if ($Architecture) { $targets = @($targets | Where-Object { $_.Arch -eq $Architecture }) }
    if ($targets.Count -eq 0) { throw "Unsupported platform/architecture combination: $Only/$Architecture (Windows ARM32 is not supported)" }
    foreach ($target in $targets) {
        if ($supported -notcontains "$($target.OS)/$($target.Arch)") {
            throw "Current Go toolchain does not support $($target.OS)/$($target.Arch)"
        }
    }
    Write-Host "WorkBuddy Gateway v$version · $toolchain · go.mod requires Go $minimumGo"
    if (-not $SkipTests) {
        $null = Invoke-Go @('vet', './...') 'vet'
        $null = Invoke-Go @('test', './...') 'tests'
    } else {
        Write-BuildAudit 'checks_skipped' 'SkipTests requested by user; code checks skipped' @{}
    }
    [IO.Directory]::CreateDirectory($output) | Out-Null
    if ($Clean) {
        $known = '^(workbuddy-gateway-(windows|linux|darwin)-(386|amd64|arm|arm64|armv[567])(\.exe)?|SHA256SUMS|build-manifest\.json|config\.example\.json)$'
        foreach ($file in Get-ChildItem -LiteralPath $output -File -Force) {
            if ($file.Name -notmatch $known) { continue }
            if ($file.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Refusing to clean linked artifact: $($file.Name)" }
            Remove-Item -LiteralPath $file.FullName -Force
            Write-BuildAudit 'artifact_cleaned' 'Cleaned one known build file; other files and subdirectories are kept' @{ file = $file.Name }
        }
    }
    $artifacts = @()
    foreach ($target in $targets) {
        Set-BuildEnvironment 'GOOS' $target.OS
        Set-BuildEnvironment 'GOARCH' $target.Arch
        Set-BuildEnvironment 'GOARM' $(if ($target.Arch -eq 'arm') { $ArmVersion } else { $null })
        Set-BuildEnvironment 'GOAMD64' 'v1'
        Set-BuildEnvironment 'GO386' 'sse2'
        Set-BuildEnvironment 'GOARM64' 'v8.0'
        $name = "workbuddy-gateway-$($target.OS)-$($target.Suffix)$($target.Ext)"
        $path = Join-Path $output $name
        if (Test-Path -LiteralPath $path) {
            $existing = Get-Item -LiteralPath $path -Force
            if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "Artifact path is not an ordinary file: $path"
            }
        }
        $temporary = Join-Path $output "$name.$trace.tmp"
        Write-Host "Building $($target.OS)/$($target.Arch) -> $name"
        try {
            $null = Invoke-Go @('build', '-trimpath', '-buildvcs=false', '-ldflags', '-s -w', '-o', $temporary, '.') "build_$($target.OS)_$($target.Arch)"
            if (-not (Test-Path -LiteralPath $temporary) -or (Get-Item -LiteralPath $temporary).Length -eq 0) { throw "No valid artifact produced: $name" }
            Move-Item -LiteralPath $temporary -Destination $path -Force
        } finally {
            if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
        }
        $artifacts += [ordered]@{
            name = $name; goos = $target.OS; goarch = $target.Arch
            goarm = $(if ($target.Arch -eq 'arm') { $ArmVersion } else { $null })
            bytes = (Get-Item -LiteralPath $path).Length
            sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        }
        Write-BuildAudit 'artifact_created' 'Artifact complete, SHA256 computed' @{ artifact = $artifacts[-1] }
    }
    $examplePath = Join-Path $root 'config.example.json'
    foreach ($name in @('config.example.json', 'build-manifest.json', 'SHA256SUMS')) {
        $destination = Join-Path $output $name
        if (Test-Path -LiteralPath $destination) {
            $existing = Get-Item -LiteralPath $destination -Force
            if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "Config or manifest output path is not an ordinary file: $destination"
            }
        }
    }
    $example = Get-Content -LiteralPath $examplePath -Raw | ConvertFrom-Json
    if ($example.gateway.adminKey -or $example.gateway.apiKey) { throw 'Example config contains secrets; refusing to copy or publish' }
    Copy-Item -LiteralPath $examplePath -Destination (Join-Path $output 'config.example.json') -Force
    $commit = 'unknown'
    $dirty = $null
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $commit = (& git rev-parse HEAD 2>$null | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) { $commit = 'unknown' }
        $dirty = [bool]((& git status --porcelain --untracked-files=no 2>$null | Out-String).Trim())
    }
    $manifest = [ordered]@{
        version = $version; source_commit = $commit; tracked_source_dirty = $dirty
        toolchain = $toolchain; minimum_go = $minimumGo
        built_at = [DateTimeOffset]::Now.ToString('o'); cgo_enabled = $false
        windows_arm32_supported = $false; artifacts = $artifacts
    }
    [IO.File]::WriteAllText((Join-Path $output 'build-manifest.json'), ($manifest | ConvertTo-Json -Depth 8) + "`n", $utf8)
    $sums = @($artifacts | ForEach-Object { "$($_.sha256)  $($_.name)" })
    foreach ($name in @('config.example.json', 'build-manifest.json')) {
        $hash = (Get-FileHash -LiteralPath (Join-Path $output $name) -Algorithm SHA256).Hash.ToLowerInvariant()
        $sums += "$hash  $name"
    }
    [IO.File]::WriteAllText((Join-Path $output 'SHA256SUMS'), ($sums -join "`n") + "`n", $utf8)
    Write-BuildAudit 'build_succeeded' 'All targets completed; build manifest and checksums written' @{ count = $artifacts.Count; output = $output; version = $version; commit = $commit }
    Write-Host "Build succeeded: $($artifacts.Count) binaries, output $output"
} catch {
    $failure = $_
    Write-BuildAudit 'build_failed' 'Build failed; this run was not marked successful' @{ error = $_.Exception.Message }
} finally {
    foreach ($key in $environmentNames) { Set-BuildEnvironment $key $oldEnvironment[$key] }
    Pop-Location
}
if ($failure) { throw $failure }
