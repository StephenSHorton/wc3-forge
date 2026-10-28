<#
.SYNOPSIS
  End-to-end release validator for wc3-forge — drives the ACTUAL built binary's
  MCP bridge through a battery of editor-workflow scenarios and reports pass/fail.

.DESCRIPTION
  This is the "artifact runner" half of the test suite (the other half is the Go
  in-process suite in internal/forge/e2e_scenarios_test.go, which is run locally
  before every PR; there is no PR CI). Run this before cutting a release to validate the real .exe — it
  exercises the same surfaces over the wire AND the CASC-dependent Object Editor
  that the bare-CI Go suite can't reach.

  Each scenario opens its OWN fresh copy of the committed folder fixture
  (internal/forge/testdata/extracted-map), so they don't interfere. The bridge
  runs headless (no window) on an isolated lock dir; only its own PID is killed
  on teardown (never a sibling/interactive wc3-forge).

.PARAMETER Bin
  Path to wc3-forge.exe. Defaults to build/bin/wc3-forge.exe under the repo root.

.PARAMETER Build
  Run `wails build` first (CGO disabled) to produce a fresh binary.

.PARAMETER SkipCasc
  Skip the CASC-tier Object Editor scenarios (use on a machine with no Warcraft
  III install — those scenarios need the stock SLK base data from CASC).

.EXAMPLE
  pwsh test/e2e/run_release.ps1
  pwsh test/e2e/run_release.ps1 -Build
  pwsh test/e2e/run_release.ps1 -Bin 'C:\Program Files\wc3-forge\wc3-forge\wc3-forge.exe' -SkipCasc
#>
[CmdletBinding()]
param(
    [string]$Bin,
    [switch]$Build,
    [switch]$SkipCasc
)

# NOTE: no Set-StrictMode here. Scenarios read the JSON-RPC envelope with the
# natural lenient style ($resp.error is $null on success because the Go envelope
# omits the key); StrictMode 'Latest' would turn every such access into a
# "property cannot be found" error. $ErrorActionPreference=Stop still aborts on
# real cmdlet failures.
$ErrorActionPreference = 'Stop'

$here = $PSScriptRoot
$repo = (Resolve-Path (Join-Path $here '..\..')).Path
if (-not $Bin) { $Bin = Join-Path $repo 'build\bin\wc3-forge.exe' }
$fixture = Join-Path $repo 'internal\forge\testdata\extracted-map'

. (Join-Path $here 'lib\bridge.ps1')

if ($Build) {
    Write-Host "Building wc3-forge (wails build, CGO disabled)..." -ForegroundColor Cyan
    Push-Location $repo
    try { $env:CGO_ENABLED = '0'; & wails build } finally { Pop-Location }
}
if (-not (Test-Path $Bin)) {
    throw "binary not found at '$Bin' — pass -Bin <path> or run with -Build to compile it."
}
if (-not (Test-Path $fixture)) {
    throw "fixture map not found at '$fixture'."
}

# Open-FreshFixture copies the committed fixture to a fresh temp dir and opens it
# over the bridge, returning the folder path (for on-disk assertions). Each
# scenario calls this so it starts from a clean, isolated map.
function Open-FreshFixture {
    param([Parameter(Mandatory)][pscustomobject]$Bridge)
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("wc3-e2e-" + [System.Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force $tmp | Out-Null
    $folder = Join-Path $tmp 'm'
    Copy-Item -Recurse $fixture $folder
    $r = Send-ForgeRpc $Bridge 'map.open' @{ path = $folder }
    if ($r.PSObject.Properties.Name -contains 'error' -and $r.error) {
        throw "Open-FreshFixture: map.open failed: $($r.error.message)"
    }
    return $folder
}

# Dot-source every scenario file (each defines an Invoke-*Scenarios function).
Get-ChildItem -Path (Join-Path $here 'scenarios') -Filter '*.ps1' | Sort-Object Name | ForEach-Object { . $_.FullName }

Reset-ForgeChecks
Write-Host "wc3-forge E2E release runner" -ForegroundColor Cyan
Write-Host "  binary : $Bin"
Write-Host "  fixture: $fixture"

$b = Start-ForgeBridge -Bin $Bin
Write-Host "  bridge : pid=$($b.Pid) port=$($b.Port)"
$ping = Send-ForgeRpc $b 'bridge.ping' @{}
$ver = if ($ping.result) { $ping.result.version } else { '?' }
Write-Host "  version: $ver"
Write-Host ""

try {
    $ctx = [pscustomobject]@{ Bridge = $b; Fixture = $fixture; Bin = $Bin; SkipCasc = [bool]$SkipCasc }

    Write-Host "[ Save-safety ]" -ForegroundColor Cyan;        Invoke-SaveSafetyScenarios $ctx
    Write-Host "[ Terrain ]" -ForegroundColor Cyan;            Invoke-TerrainScenarios $ctx
    Write-Host "[ Units + start locations ]" -ForegroundColor Cyan; Invoke-UnitsScenarios $ctx
    Write-Host "[ Regions + imports ]" -ForegroundColor Cyan;  Invoke-RegionsImportsScenarios $ctx
    Write-Host "[ History + view + diagnostics ]" -ForegroundColor Cyan; Invoke-HistoryViewDiagScenarios $ctx
    if ($SkipCasc) {
        Write-Host "[ Object Editor ] SKIPPED (-SkipCasc; needs a Warcraft III/CASC install)" -ForegroundColor Yellow
    } else {
        Write-Host "[ Object Editor (CASC) ]" -ForegroundColor Cyan; Invoke-ObjectScenarios $ctx
    }
} finally {
    Stop-ForgeBridge $b
}

$fails = Write-ForgeSummary
exit $fails
