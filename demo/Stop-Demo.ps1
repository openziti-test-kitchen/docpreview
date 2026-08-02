<#
.SYNOPSIS
    Stop the demo daemon and anything it left running.

.DESCRIPTION
    A killed build leaves its node process behind, and that process holds the workspace directory open — which
    is what makes a later reseed fail with "device or resource busy". So node goes too.

    Only the node processes this demo started. Matching on the command line, rather than stopping every
    process named node, is the difference between cleaning up after yourself and clearing the room: a plain
    `Get-Process -Name node | Stop-Process -Force` kills any unrelated node process on the machine too.
#>
[CmdletBinding()]
param(
    [switch] $KeepNode
)

$ErrorActionPreference = 'Stop'

$daemons = Get-Process -Name docpreview -ErrorAction SilentlyContinue
if ($daemons) {
    $daemons | Stop-Process -Force
    Write-Host "stopped docpreview (PID $($daemons.Id -join ', '))"
} else {
    Write-Host 'docpreview was not running'
}

if (-not $KeepNode) {
    # Builds run inside the demo's own data_dir, so a node process working there is ours and one working
    # anywhere else is not. Get-CimInstance rather than Get-Process because only the CIM view carries the
    # command line, which is the only place that distinction is visible.
    $demoRoot = (Join-Path $PSScriptRoot 'data')
    $ours = Get-CimInstance Win32_Process -Filter "Name = 'node.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -and $_.CommandLine -like "*$demoRoot*" }

    if ($ours) {
        foreach ($p in $ours) {
            Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
        }
        Write-Host "stopped $($ours.Count) node process(es) under $demoRoot"
    } else {
        Write-Host 'no demo node processes to clean up'
    }

    # Anything left is somebody else's. Say so rather than killing it: this script must not stop a node
    # process outside its own data directory.
    $others = Get-CimInstance Win32_Process -Filter "Name = 'node.exe'" -ErrorAction SilentlyContinue |
        Where-Object { -not $_.CommandLine -or $_.CommandLine -notlike "*$demoRoot*" }
    if ($others) {
        Write-Host "left $($others.Count) unrelated node process(es) alone (PID $($others.ProcessId -join ', '))"
    }
}
