<#
.SYNOPSIS
    Stop the live instance, rebuild, start all three processes, and wait until it is ready.

.DESCRIPTION
    The cycle every change to the daemon needs, in one command, because doing it by hand is six steps in a
    fixed order and getting the order wrong fails in ways that look like bugs in the code:

      1. Stop every docpreview process. A running daemon holds build.claude\docpreview.exe open, and the
         rebuild then fails with "used by another process" — the single most common self-inflicted failure
         in this repository.
      2. Build. `-o build.claude\` is enforced by a hook, and the package list is explicit because
         `./...` picks up the Go files inside demo\node_modules, which are not part of this module.
      3. Start `serve`, then the two zrok shares. The shares forward to the daemon and log errors until it
         answers, so the daemon goes first.
      4. Wait for `/status` to report that recovery has finished, printing the stages.

    Skipping step 1 is the "used by another process" build failure. Skipping step 4 is worse: a restart
    republishes every preview, and querying /status inside that window returns an empty event list, which
    has been mistaken for data loss more than once.

    This is for the live instance in .docpreview\. The demo has its own scripts under demo\.

.PARAMETER NoBuild
    Restart without rebuilding. For when the binary is already current and only the processes need
    bouncing — a config change, or recovering from a killed share.

.PARAMETER NoShares
    Start the daemon only. Useful when working on something that does not need the tunnels, since each
    share is a zrok round trip.

.PARAMETER NoWait
    Return as soon as the processes are started, without waiting for recovery.

.PARAMETER Prefix
    Set the installation's hostname prefix after the daemon is up, then report it. Two installations
    sharing one zrok account collide on names unless they differ here — see docs/design/16-exposer-zrok.md.

.EXAMPLE
    .\Restart-Docpreview.ps1
    The whole cycle: stop, build, start, wait.

.EXAMPLE
    .\Restart-Docpreview.ps1 -NoBuild -NoShares
    Bounce just the daemon, no rebuild, no tunnels.

.EXAMPLE
    .\Restart-Docpreview.ps1 -Prefix a
    Restart and set the hostname prefix, so this installation publishes a-<name>.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [string] $Config = '.docpreview\config.yml',
    [string] $Base = 'http://127.0.0.1:8471',
    [string] $WebhookName = 'docpreview',
    [string] $DashboardName = 'docpreview-dash',
    [string] $Prefix,
    [switch] $NoBuild,
    [switch] $NoShares,
    [switch] $NoWait
)

$ErrorActionPreference = 'Stop'
Set-Location $Root

$exe = Join-Path $Root 'build.claude\docpreview.exe'

# ── 1. Stop ───────────────────────────────────────────────────────────────────────────────────────
# Every one of them, not just the daemon: the two shares hold no lock on the binary but they forward to
# a daemon that is about to be replaced, and a share left pointing at a dead upstream logs an error per
# request until somebody notices.
$running = Get-Process -Name docpreview -ErrorAction SilentlyContinue
if ($running) {
    Write-Host "stopping $($running.Count) docpreview process(es)"
    $running | Stop-Process -Force
    # The file handle is released asynchronously, so a build started immediately after Stop-Process can
    # still lose the race. Waiting for the processes to actually exit is cheaper than the confusing
    # failure that follows.
    $running | Wait-Process -Timeout 20 -ErrorAction SilentlyContinue
} else {
    Write-Host 'nothing was running'
}

# ── 2. Build ──────────────────────────────────────────────────────────────────────────────────────
if (-not $NoBuild) {
    Write-Host 'building'
    # The explicit package list is not a style choice: demo\node_modules contains Go files belonging to
    # other modules, so `./...` fails outright.
    & go build -o build.claude/ ./cmd/... ./internal/...
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }
}
if (-not (Test-Path $exe)) { throw "$exe does not exist — build first" }

# ── 3. Start ──────────────────────────────────────────────────────────────────────────────────────
# The daemon writes its own log file when the config sets log_file, so nothing is redirected here.
Write-Host 'starting the daemon'
$daemon = Start-Process -PassThru -WindowStyle Hidden -FilePath $exe `
    -ArgumentList 'serve', '-config', $Config
Write-Host "  serve: PID $($daemon.Id)"

if (-not $NoShares) {
    # -zrok-name, never -config: these two read no config file and hold no copy of the webhook secret,
    # which is the whole reason webhook-only exists. Passing -config exits 2 with a usage dump.
    Write-Host 'starting the shares'
    $webhook = Start-Process -PassThru -WindowStyle Hidden -FilePath $exe `
        -ArgumentList 'webhook-only', '-zrok-name', $WebhookName,
                      '-path', '/webhook/github', '-path', '/webhook/bitbucket'
    Write-Host "  webhook-only: PID $($webhook.Id)"

    $dash = Start-Process -PassThru -WindowStyle Hidden -FilePath $exe `
        -ArgumentList 'dashboard-only', '-zrok-name', $DashboardName
    Write-Host "  dashboard-only: PID $($dash.Id)"
}

# ── 4. Wait ───────────────────────────────────────────────────────────────────────────────────────
if (-not $NoWait) {
    & (Join-Path $Root 'demo\Wait-Docpreview.ps1') -Base $Base
    if ($LASTEXITCODE -ne 0) { throw "the daemon did not become ready (exit $LASTEXITCODE)" }
}

# ── 5. The prefix, if asked ───────────────────────────────────────────────────────────────────────
# After the wait, because the route is served by the daemon and a PUT during recovery would be answered
# by nothing. Stored in the database rather than the config file, which is why it is set through the API
# rather than written here.
if ($PSBoundParameters.ContainsKey('Prefix')) {
    $body = @{ prefix = $Prefix } | ConvertTo-Json -Compress
    $st = Invoke-RestMethod -Method Put -Uri "$Base/api/settings/prefix" -Body $body -ContentType 'application/json'
    Write-Host "hostname prefix: '$($st.defaults.prefix)'"
    Write-Host '  previews already published keep their names until rebuilt'
}

Write-Host ''
Write-Host "dashboard: $Base/"
