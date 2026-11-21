[CmdletBinding()]
param(
    [string]$Core = (Join-Path $PSScriptRoot '..\build\core\ProxyBridgeCore.dll'),
    [switch]$UseBuiltTests,
    [switch]$Outbound
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$out = Join-Path $root '.cache\e2e'
New-Item -ItemType Directory -Force -Path $out | Out-Null
$reportName = if ($Outbound) { 'windows-e2e-outbound.log' } else { 'windows-e2e.log' }
$report = Join-Path $out $reportName
'GAMEBOOST Windows end-to-end' | Set-Content -LiteralPath $report -Encoding utf8

try {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'WinDivert end-to-end tests require an administrator token.'
    }
    if ((Get-Service -Name WinDivert -ErrorAction SilentlyContinue).Status -eq 'Running') {
        throw 'WinDivert is already active. Stop the current acceleration session before running these tests.'
    }
    $env:MSBOOST_CORE_DLL = (Resolve-Path -LiteralPath $Core).Path
    $env:GAMEBOOST_E2E = '1'
    if ($Outbound) { $env:GAMEBOOST_E2E_OUTBOUND = '1' }
    $routes = Join-Path $out 'routes.test.exe'
    $dns = Join-Path $out 'dns.test.exe'
    if (-not $UseBuiltTests) {
        Push-Location -LiteralPath $root
        try {
            go test -c -o $routes ./internal/app
            if ($LASTEXITCODE -ne 0) { throw 'Routing test build failed.' }
            go test -c -o $dns ./internal/dnsroute
            if ($LASTEXITCODE -ne 0) { throw 'DNS test build failed.' }
        } finally { Pop-Location }
    }
    Push-Location -LiteralPath $root
    try {
        & $routes '-test.run=^TestWindowsRoutingE2E$' '-test.v' '-test.timeout=75s' 2>&1 |
            Tee-Object -FilePath $report -Append
        $routeExit = $LASTEXITCODE
        & $dns '-test.run=^TestWindowsDNSE2E$' '-test.v' '-test.timeout=30s' 2>&1 |
            Tee-Object -FilePath $report -Append
        if ($routeExit -ne 0 -or $LASTEXITCODE -ne 0) { throw 'One or more end-to-end tests failed.' }
    } finally { Pop-Location }
    'GAMEBOOST_E2E_PASS' | Add-Content -LiteralPath $report
    exit 0
} catch {
    "GAMEBOOST_E2E_FAIL $($_.Exception.Message)" | Add-Content -LiteralPath $report
    Write-Error -ErrorAction Continue $_.Exception.Message
    exit 1
}
