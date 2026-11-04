<#
.SYNOPSIS
    Runs Go, web, and build script tests in one place; never starts production services.
.DESCRIPTION
    Runs all three test suites by default; stops at the first failing step and never treats a missing tool as a pass.
    Restores the caller's working directory and Go environment; audit is written to the project logs/.
.PARAMETER Suite
    all (default), go, web, or build.
.EXAMPLE
    .\scripts\test.ps1
    .\scripts\test.ps1 -Suite go
#>
param(
    [ValidateSet('all', 'go', 'web', 'build')]
    [string]$Suite = 'all'
)

$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$trace = [Guid]::NewGuid().ToString('N')
$encoding = [Text.UTF8Encoding]::new($false)
$logDirectory = Join-Path $root 'logs'
[IO.Directory]::CreateDirectory($logDirectory) | Out-Null
$auditPath = Join-Path $logDirectory ("test-runner-{0}.jsonl" -f (Get-Date -Format 'yyyy-MM-dd'))
$environmentNames = @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOARM', 'GOAMD64', 'GO386', 'GOARM64',
    'WORKBUDDY_BROWSER_FIXTURE', 'WORKBUDDY_HEALTH_BROWSER_FIXTURE', 'WORKBUDDY_BROWSER_DURATION')
$previousEnvironment = @{}
foreach ($name in $environmentNames) {
    $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

function Protect-TestAudit {
    param([string]$Text)
    $Text = $Text -replace 'github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9]+', '[REDACTED]'
    $Text = $Text -replace '(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+', '$1[REDACTED]'
    return $Text -replace '(https?://)[^/\s:@]+:[^@\s/]+@', '$1[REDACTED]@'
}

function Write-TestAudit {
    param([string]$Event, [string]$Description, [hashtable]$Fields = @{})
    $record = [ordered]@{
        time = [DateTimeOffset]::Now.ToString('o')
        trace_id = $trace
        event = $Event
        description = $Description
    }
    foreach ($key in $Fields.Keys) { $record[$key] = $Fields[$key] }
    $line = Protect-TestAudit ($record | ConvertTo-Json -Compress -Depth 8)
    [IO.File]::AppendAllText($auditPath, $line + "`n", $encoding)
}

function Invoke-TestTool {
    param([string]$Tool, [string[]]$Arguments, [string]$Phase)
    $command = Get-Command $Tool -ErrorAction Stop
    $PSNativeCommandUseErrorActionPreference = $false
    Write-TestAudit 'tool_started' 'Test phase started; only command arguments are recorded, no environment variables printed' @{
        phase = $Phase; tool = $Tool; arguments = $Arguments
    }
    $output = @(& $command @Arguments 2>&1)
    $code = $LASTEXITCODE
    foreach ($line in $output) { Write-Host (Protect-TestAudit ([string]$line)) }
    Write-TestAudit 'tool_completed' 'Test phase finished; output redacted and written to disk' @{
        phase = $Phase; exit_code = $code; output = ($output -join "`n")
    }
    if ($code -ne 0) { throw "$Phase tests failed, exit code $code; see $auditPath" }
}

Push-Location -LiteralPath $root
try {
    Write-TestAudit 'tests_requested' 'Running local tests; no deploy, no push, no real price probing' @{ suite = $Suite }
    if ($Suite -in @('all', 'go')) {
        foreach ($name in $environmentNames) {
            [Environment]::SetEnvironmentVariable($name, $null, 'Process')
        }
        [Environment]::SetEnvironmentVariable('CGO_ENABLED', '0', 'Process')
        Invoke-TestTool 'go' @('vet', './...') 'go_vet'
        Invoke-TestTool 'go' @('test', './...', '-count=1') 'go_tests'
    }
    if ($Suite -in @('all', 'web')) {
        Invoke-TestTool 'node' @('--check', 'web/app.js') 'web_syntax'
        Invoke-TestTool 'node' @('--test', 'tests/web/app.test.cjs') 'web_tests'
    }
    if ($Suite -in @('all', 'build')) {
        Write-TestAudit 'build_tests_started' 'Invoking mocked build regression; no real cross-compilation is triggered' @{}
        & (Join-Path $root 'tests/build/build_script.test.ps1')
        Write-TestAudit 'build_tests_completed' 'Build matrix, cleaning boundary, and environment restore tests passed' @{}
    }
    Write-TestAudit 'tests_completed' 'All selected tests passed' @{ suite = $Suite }
    # Mocked build tests include expected nonzero exit codes; that state must not leak into a whole-suite failure.
    $global:LASTEXITCODE = 0
    Write-Host "All tests passed; audit: $auditPath"
} catch {
    $global:LASTEXITCODE = 1
    Write-TestAudit 'tests_failed' 'Tests failed or a tool is missing; no failing stage was skipped' @{
        suite = $Suite; error = $_.Exception.Message
    }
    throw
} finally {
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process')
    }
    Pop-Location
}
