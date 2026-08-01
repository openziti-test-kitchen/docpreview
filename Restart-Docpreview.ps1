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
      4. Wait for `/readyz` to report that recovery has finished, printing the stages.

    Skipping step 1 is the "used by another process" build failure. Skipping step 4 is worse: a restart
    republishes every preview, and querying the daemon inside that window returns an empty event list,
    which has been mistaken for data loss more than once.

    This is for the live instance in .docpreview\. The demo has its own scripts under demo\.

.PARAMETER NoBuild
    Restart without rebuilding. For when the binary is already current and only the processes need
    bouncing — a config change, or recovering from a killed share.

.PARAMETER NoShares
    Start the daemon only. Useful when working on something that does not need the tunnels, since each
    share is a zrok round trip.

.PARAMETER NoWait
    Return as soon as the processes are started, without waiting for recovery.

.PARAMETER Idle
    Wait for every queued build to finish too, not just for recovery. A restart queues work of its own —
    the startup scan builds any open pull request with no preview — so use this when you want the daemon
    quiet before looking at anything.

.PARAMETER Prefix
    Set the installation's hostname prefix, before the daemon starts. Two installations sharing one zrok
    account collide on names unless they differ here — see docs/design/16-exposer-zrok.md.

.PARAMETER Oauth
    Gate the dashboard share at the zrok frontend: `google` or `github`. Needs -OauthDomain. Previews are
    deliberately never gated — a preview URL goes in a pull request for anyone reviewing to open.

.PARAMETER OauthDomain
    Comma-separated email domains allowed through -Oauth, e.g. netfoundry.io.

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
    [string] $Oauth,
    [string] $OauthDomain,
    [switch] $NoBuild,
    [switch] $NoShares,
    [switch] $NoWait,
    [switch] $Idle
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

# ── 2b. The prefix, if asked ──────────────────────────────────────────────────────────────────────
# Before the daemon starts, and through the CLI rather than the API.
#
# It used to be an authenticated PUT after the wait. Once the login went in front of the whole
# dashboard that PUT was answered with a redirect to a form, and the script reported a prefix it had
# not set. The CLI writes the same database row the API route does, and the daemon reads the prefix
# once at startup — so setting it here is the ordering that actually works.
if ($PSBoundParameters.ContainsKey('Prefix')) {
    & $exe settings prefix $Prefix -config $Config
    if ($LASTEXITCODE -ne 0) { throw "could not set the prefix to '$Prefix'" }
}

# ── 3. Start ──────────────────────────────────────────────────────────────────────────────────────
# Every process gets its output on disk.
#
# The first version started all three with -WindowStyle Hidden and no redirection, which threw
# their output away. `dashboard-only` then died three times in one afternoon and left no evidence
# at all — the share 502s, the process is gone, and there is nothing to read. A hidden process
# whose output goes nowhere cannot be diagnosed, only guessed at.
#
# The daemon writes its own file as well when the config sets log_file; this catches anything that
# never reaches slog, which is exactly the class of thing that kills a process.
$logDir = Join-Path $Root '.docpreview\logs-process'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

function Start-Docpreview([string] $Label, [string[]] $Arguments) {
    $out = Join-Path $logDir "$Label.out.log"
    $err = Join-Path $logDir "$Label.err.log"
    $p = Start-Process -PassThru -WindowStyle Hidden -FilePath $exe `
        -ArgumentList $Arguments -RedirectStandardOutput $out -RedirectStandardError $err
    Write-Host "  ${Label}: PID $($p.Id)  ->  $err"
    return $p
}

Write-Host 'starting the daemon'
$daemon = Start-Docpreview 'serve' @('serve', '-config', $Config)

if (-not $NoShares) {
    # -zrok-name, never -config: these two read no config file and hold no copy of the webhook secret,
    # which is the whole reason webhook-only exists. Passing -config exits 2 with a usage dump.
    Write-Host 'starting the shares'
    # -zrok-home when this installation has its own zrok environment. These two read no config
    # file, so they cannot derive it — and a share created from a different zrok account than the
    # daemon's reserves a name the previews cannot use. Absent, they use the machine's ~/.zrok2,
    # which is right for the setup here and wrong the moment `docpreview zrok use project` is run.
    $zrokArgs = @()
    $zrokScope = (& $exe zrok status -config $Config 2>$null | Select-String '^in use: (\w+)').Matches.Groups[1].Value
    if ($zrokScope -eq 'project') {
        $zrokArgs = @('-zrok-home', (Join-Path $Root '.docpreview\data\zrok2'))
        Write-Host "  zrok environment: this installation's own"
    }

    $webhook = Start-Docpreview 'webhook-only' (@(
        'webhook-only', '-zrok-name', $WebhookName,
        '-path', '/webhook/github', '-path', '/webhook/bitbucket') + $zrokArgs)

    # The dashboard share, optionally gated at the zrok frontend. Only this one: the webhook share
    # is called by GitHub and Bitbucket, which hold no account with any OAuth provider, and the
    # preview shares must stay open for reviewers.
    $dashArgs = @('dashboard-only', '-zrok-name', $DashboardName) + $zrokArgs
    if ($Oauth)       { $dashArgs += @('-oauth', $Oauth) }
    if ($OauthDomain) { $dashArgs += @('-oauth-domain', $OauthDomain) }

    $dash = Start-Docpreview 'dashboard-only' $dashArgs
    if ($Oauth) { Write-Host "    gated at the zrok frontend: $Oauth / $OauthDomain" }
}

# ── 4. Wait ───────────────────────────────────────────────────────────────────────────────────────
if (-not $NoWait) {
    # -Idle, because a restart now queues work of its own: the startup scan builds any open pull
    # request with no preview and any project with no branch preview. Returning at "ready" leaves
    # several minutes of building behind, and the caller then hand-rolls a poll loop — which is
    # the thing this script exists to stop.
    # A hashtable, not an array. Splatting an array passes its elements *positionally*, so
    # @('-Base', $Base) put the literal string "-Base" into the first parameter and the URL into
    # the second — which is an int, and the error names TimeoutSeconds rather than anything a
    # reader would connect to this line.
    $waitArgs = @{ Base = $Base }
    if ($Idle) { $waitArgs['Idle'] = $true }
    & (Join-Path $Root 'demo\Wait-Docpreview.ps1') @waitArgs
    if ($LASTEXITCODE -ne 0) { throw "the daemon did not become ready (exit $LASTEXITCODE)" }
}

# ── 5. Is everything still alive? ─────────────────────────────────────────────────────────────────
# Asked explicitly, because a share that dies quietly looks like nothing at all until somebody
# opens the URL and gets a 502 — which happened three times before this check existed. The zrok
# share record outlives the process holding it, so the frontend keeps routing to a backend that is
# gone.
$expected = @{ 'serve' = $daemon }
if (-not $NoShares) {
    $expected['webhook-only'] = $webhook
    $expected['dashboard-only'] = $dash
}

$dead = @()
foreach ($name in $expected.Keys) {
    $p = $expected[$name]
    if ($null -eq $p -or $p.HasExited) {
        $dead += $name
        $err = Join-Path $logDir "$name.err.log"
        Write-Host ""
        Write-Host "$name has already exited." -ForegroundColor Red
        if (Test-Path $err) {
            Write-Host "  last lines of $err" -ForegroundColor DarkGray
            Get-Content $err -Tail 15 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
        }
    }
}
if ($dead.Count -gt 0) {
    throw "these did not stay running: $($dead -join ', ')"
}

Write-Host ''
Write-Host "dashboard: $Base/"
Write-Host "logs: $logDir"
