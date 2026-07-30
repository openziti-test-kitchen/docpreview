<#
.SYNOPSIS
    Wait until the daemon has finished starting, printing what it is doing while you wait.

.DESCRIPTION
    Startup is not instant and not quiet about it. The daemon reaps every share the previous process left
    behind and then republishes one share per preview plus one per retained build — ten to fifteen seconds
    each against the hosted zrok controller, so restoring two pull requests is minutes during which every
    preview URL 404s and a queued build cannot start.

    This blocks until `/status` reports `starting: false`, printing each stage and its progress as it goes.
    The same numbers the dashboard banner shows, for when you are in a terminal rather than a browser.

    Exits 0 when the daemon is ready, 1 on timeout, 2 if it never answered at all. A daemon that is not
    running is the 2 case, and it is worth telling apart from a slow one: nothing will change if nothing
    is listening.

.PARAMETER Base
    The daemon's own listener. Not a tunnel: `/status` is served on the loopback address in the config.

.PARAMETER TimeoutSeconds
    How long to wait before giving up. The default is generous because the thing being waited on is a
    remote controller under load, and a timeout here reads as a fault when it is usually just slow.

.PARAMETER Quiet
    Print nothing; just set the exit code.

.EXAMPLE
    .\demo\Wait-Docpreview.ps1
    Waits on the live instance, printing progress.

.EXAMPLE
    .\demo\Wait-Docpreview.ps1 -Base http://127.0.0.1:8493 -Quiet
    Waits on the demo daemon silently, for use in a script.
#>
[CmdletBinding()]
param(
    [string] $Base = 'http://127.0.0.1:8471',
    [int]    $TimeoutSeconds = 600,
    [int]    $IntervalSeconds = 3,
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
        $status = Invoke-RestMethod -Uri "$Base/status" -TimeoutSec 5
        $answered = $true
    } catch {
        # Not up yet, or not up at all. Both look the same from here, and the difference is
        # only knowable by giving up — so keep polling and report it at the end.
        Write-Line "waiting for $Base to answer…" 'DarkGray'
        Start-Sleep -Seconds $IntervalSeconds
        continue
    }

    if (-not $status.starting) {
        $secs = [int]((Get-Date) - $started).TotalSeconds
        $ready = @($status.previews | Where-Object { $_.state -eq 'ready' -and $_.url }).Count
        Write-Line ("ready after {0:mm\m\ ss\s} — {1} preview(s) serving, {2} queued, {3} building" -f
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
