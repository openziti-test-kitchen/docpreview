<#
.SYNOPSIS
    Report or change which exposer publishes previews, and restart into it.

.DESCRIPTION
    `exposer.kind` is a stored setting the daemon reads once at startup, so changing it is two acts:
    record the choice, then restart. Doing that by hand is a login, a POST and a restart script, and
    getting the second half wrong leaves the panel describing two different answers at once.

    **This is destructive to URLs.** On restart every preview is republished at a new address and
    every open pull request comment is rewritten to match. That is not a side effect to discover —
    it is the whole outcome — so the switch asks unless -Yes is passed. An unconfirmed switch on the
    live instance rewrites the comment on every open pull request it has.

.PARAMETER Kind
    zrok2, frontdoor, ziti or local. Omit to report the current state and exit.

.PARAMETER Restart
    Restart the daemon afterwards, which is what actually applies it.

.PARAMETER Yes
    Skip the confirmation prompt. It does **not** imply -Live.

.PARAMETER Live
    Required to change the instance on :8471. That one comments on real pull requests, and every
    switch rewrites all of them — so trying an exposer out belongs on the demo daemon, which has no
    repositories behind it.

.EXAMPLE
    .\Set-Exposer.ps1
    What is running, what is stored, and whether a restart is owed.

.EXAMPLE
    .\Set-Exposer.ps1 -Base http://127.0.0.1:8493 -Kind ziti -Restart
    Try OpenZiti on the demo daemon, where nothing comments on anything.

.EXAMPLE
    .\Set-Exposer.ps1 -Kind zrok2 -Restart -Yes -Live
    Put the live instance back on zrok. This is the undo.
#>
[CmdletBinding()]
param(
    [ValidateSet('zrok2', 'frontdoor', 'ziti', 'local')]
    [string] $Kind,
    [string] $Root = $PSScriptRoot,
    [string] $Base = 'http://127.0.0.1:8471',
    [string] $Password = $env:DOCPREVIEW_PASSWORD,
    [switch] $Restart,
    [switch] $Yes,
    [switch] $Live
)

$ErrorActionPreference = 'Stop'
Set-Location $Root
. (Join-Path $Root 'Invoke-Docpreview.ps1')

# The live instance needs -Live said out loud.
#
# The switch itself is reversible, but the cost is on the way out: this installation comments on
# real pull requests, and every switch rewrites all of them — twice, once going and once coming
# back. That churn lands in somebody else's notifications.
#
# The demo daemon on :8493 exists for trying an exposer out and has no repositories behind it. -Yes
# deliberately does not imply -Live: unattended is a reason to be *more* careful about which daemon
# is being changed, not less.
$isLive = $Base -match ':8471(/|$)'
if ($Kind -and $isLive -and -not $Live) {
    throw "refusing to switch the live instance at $Base without -Live. It comments on real pull " +
          "requests and every switch rewrites all of them. Point -Base at the demo " +
          "(http://127.0.0.1:8493), or pass -Live if the churn is intended."
}

function Show-State {
    $s = Invoke-Docpreview GET /api/zrok -Base $Base -Password $Password
    if ($s.error) { throw $s.error }

    Write-Host "running : $($s.exposer_kind)"
    if ($s.exposer_stored -and $s.exposer_stored -ne $s.exposer_kind) {
        Write-Host "stored  : $($s.exposer_stored)  — restart to apply" -ForegroundColor Yellow
    } elseif ($s.exposer_stored) {
        Write-Host "stored  : $($s.exposer_stored)"
    } else {
        Write-Host 'stored  : nothing — the config file decides'
    }

    foreach ($o in $s.others) {
        $mark = if ($o.in_use) { '*' } else { ' ' }
        $state = if ($o.needs) { "needs $($o.needs)" } else { 'configured' }
        Write-Host ("{0} {1,-10} {2}" -f $mark, $o.kind, $state)
    }
    $zrokState = if ($s.enrolled) { 'enrolled' } else { 'no account' }
    $zrokMark = if ($s.exposer_kind -eq 'zrok2') { '*' } else { ' ' }
    Write-Host ("{0} {1,-10} {2}" -f $zrokMark, 'zrok2', $zrokState)
    return $s
}

$state = Show-State
if (-not $Kind) { return }

if ($state.exposer_stored -eq $Kind -and $state.exposer_kind -eq $Kind) {
    Write-Host ''
    Write-Host "already publishing with $Kind"
    return
}

if (-not $Yes) {
    Write-Host ''
    Write-Host 'Switching exposer republishes every preview at a new address and rewrites the' -ForegroundColor Yellow
    Write-Host 'link in every open pull request comment, on the next restart.' -ForegroundColor Yellow
    if ($Kind -eq 'local') {
        Write-Host 'local publishes 127.0.0.1 URLs, which no reviewer elsewhere can open.' -ForegroundColor Yellow
    }
    $answer = Read-Host "Switch to $Kind? (y/N)"
    if ($answer -notmatch '^(y|yes)$') {
        Write-Host 'nothing changed'
        return
    }
}

$resp = Invoke-Docpreview POST /api/zrok/exposer -Body @{ kind = $Kind } -Base $Base -Password $Password
if ($resp.error) { throw "the daemon refused it: $($resp.error)" }
Write-Host ''
Write-Host $resp.note -ForegroundColor Green

if (-not $Restart) {
    Write-Host 'Apply it with:  .\Restart-Docpreview.ps1 -NoBuild'
    return
}

& (Join-Path $Root 'Restart-Docpreview.ps1') -NoBuild -Idle
Write-Host ''
Show-State | Out-Null
