<#
.SYNOPSIS
    Wait until the daemon has finished starting, printing what it is doing while you wait.

.DESCRIPTION
    Startup is not instant and not quiet about it. The daemon reaps every share the previous process left
    behind and then republishes one share per preview plus one per retained build — ten to fifteen seconds
    each against the hosted zrok controller, so restoring two pull requests is minutes during which every
    preview URL 404s and a queued build cannot start.

    This blocks until `/readyz` reports `starting: false`, printing each stage and its progress as it goes.
    The same numbers the dashboard banner shows, for when you are in a terminal rather than a browser.

    `/readyz`, not `/status`: the dashboard requires a login, and `/status` sits behind it along with
    everything else, so polling `/status` here would be redirected to the login form and wait forever.
    `/readyz` is the open readiness surface and carries counts only, which is all this script needs.

    Exits 0 when the daemon is ready, 1 on timeout, 2 if it never answered at all. A daemon that is not
    running is the 2 case, and it is worth telling apart from a slow one: nothing will change if nothing
    is listening.

.PARAMETER Base
    The daemon's own listener. Not a tunnel: `/readyz` is served on the loopback address in the config.

.PARAMETER TimeoutSeconds
    How long to wait before giving up. The default is generous because the thing being waited on is a
    remote controller under load, and a timeout here reads as a fault when it is usually just slow.

.PARAMETER Idle
    Keep waiting until nothing is building or queued, not just until recovery finishes. Use this after a
    restart that will queue work of its own — the startup scan builds any open pull request with no preview
    and any project with no branch preview, so "ready" is often followed by minutes of building.

.PARAMETER Quiet
    Print nothing; just set the exit code.

.EXAMPLE
    .\demo\Wait-Docpreview.ps1
    Waits on the live instance, printing progress.

.EXAMPLE
    .\demo\Wait-Docpreview.ps1 -Base http://127.0.0.1:8493 -Quiet
    Waits on the demo daemon silently, for use in a script.

.EXAMPLE
    .\demo\Wait-Docpreview.ps1 -Idle
    Waits until the daemon has started *and* every queued build has finished.
#>
[CmdletBinding()]
param(
    [string] $Base = 'http://127.0.0.1:8471',
    [int]    $TimeoutSeconds = 600,
    [int]    $IntervalSeconds = 3,
    [switch] $Idle,
    [switch] $Quiet
)

$ErrorActionPreference = 'Stop'

function Write-Line([string] $Text, [string] $Colour = 'Gray') {
    if (-not $Quiet) { Write-Host $Text -ForegroundColor $Colour }
}

$deadline = (Get-Date).AddSeconds($TimeoutSeconds)
$started = Get-Date
$answered = $false
$last = ''

while ((Get-Date) -lt $deadline) {
    try {
        # -TimeoutSec below the poll interval, so a hung request cannot stretch the loop.
        $status = Invoke-RestMethod -Uri "$Base/readyz" -TimeoutSec 5
        $answered = $true
    } catch {
        # Not up yet, or not up at all. Both look the same from here, and the difference is
        # only knowable by giving up — so keep polling and report it at the end.
        Write-Line "waiting for $Base to answer…" 'DarkGray'
        Start-Sleep -Seconds $IntervalSeconds
        continue
    }

    if (-not $status.starting) {
        # -Idle keeps waiting until the queue drains as well.
        #
        # Recovery finishing and the daemon being *quiet* are different moments, and every
        # script that wanted the second one was hand-rolling `until the daemon says running: 0`.
        # A startup scan queues builds of its own — a missed webhook delivery, a branch
        # preview that never existed — so "ready" is routinely followed by several minutes of
        # building.
        if ($Idle -and ($status.running -gt 0 -or $status.pending -gt 0)) {
            $line = "building: $($status.running) running, $($status.pending) queued"
            if ($line -ne $last) {
                $last = $line
                Write-Line $line 'Yellow'
            }
            Start-Sleep -Seconds $IntervalSeconds
            continue
        }

        $secs = [int]((Get-Date) - $started).TotalSeconds
        # Counted by the daemon rather than here: /readyz reports how many previews are serving and
        # deliberately not which ones, since it answers without a login.
        $ready = $status.ready
        $what = if ($Idle) { 'idle' } else { 'ready' }
        Write-Line ("$what after {0:mm\m\ ss\s} — {1} preview(s) serving, {2} queued, {3} building" -f
            [timespan]::FromSeconds($secs), $ready, $status.pending, $status.running) 'Green'
        exit 0
    }

    # One line per change, not per poll. At three seconds a tick an unchanged stage would
    # produce a screen of identical lines and bury the one that moved.
    $p = $status.startup
    if ($p) {
        $line = if ($p.total -gt 0) { "$($p.stage): $($p.done) of $($p.total)" } else { "$($p.stage)…" }
        if ($line -ne $last) {
            $last = $line
            $note = if ($p.note) { "  $($p.note)" } else { '' }
            Write-Line "$line$note" 'Yellow'
        }
    } else {
        Write-Line 'starting…' 'Yellow'
    }

    Start-Sleep -Seconds $IntervalSeconds
}

if (-not $answered) {
    Write-Line "nothing answered at $Base in ${TimeoutSeconds}s — is the daemon running?" 'Red'
    Write-Line '  .\build.claude\docpreview.exe serve -config .docpreview\config.yml' 'DarkGray'
    exit 2
}

Write-Line "still starting after ${TimeoutSeconds}s — check the daemon's own output" 'Red'
exit 1
