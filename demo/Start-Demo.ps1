<#
.SYNOPSIS
    Start the docpreview daemon for the demo and open the dashboard.

.DESCRIPTION
    Runs in the foreground so Ctrl-C stops it. Use -Background to detach.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [switch] $Background,
    [switch] $NoOpen
)

$ErrorActionPreference = 'Stop'

$dp = & (Join-Path $PSScriptRoot 'Resolve-Docpreview.ps1') -Root $Root
$cfg = Join-Path $Root 'config.yml'

# The demo vault is opened with a fixed passphrase. Fine here and nowhere else: everything under demo\ is
# throwaway, and the alternative is every script prompting. Real deployments mint a key with
# `docpreview vault keygen`.
if (-not $env:DOCPREVIEW_MASTER_KEY) { $env:DOCPREVIEW_MASTER_KEY = 'flow-demo-passphrase' }

$running = Get-Process -Name docpreview -ErrorAction SilentlyContinue
if ($running) {
    throw "docpreview is already running (PID $($running.Id -join ', ')) — stop it with .\Stop-Demo.ps1"
}

if (-not $NoOpen) {
    # Opened before the daemon starts because the foreground case never returns. The dashboard reconnects on
    # its own, so a browser that arrives a second early is fine.
    Start-Process 'http://127.0.0.1:8493/'
}

if ($Background) {
    $p = Start-Process -PassThru -NoNewWindow -FilePath $dp `
        -ArgumentList 'serve', '-config', $cfg `
        -RedirectStandardError  (Join-Path $Root 'daemon.log') `
        -RedirectStandardOutput (Join-Path $Root 'daemon.out')
    Write-Host "docpreview serving on http://127.0.0.1:8493/ (PID $($p.Id))"
    # slog writes to stderr, so that is the file worth naming. daemon.out catches anything that reaches
    # stdout, which should be nothing.
    Write-Host "logs: $(Join-Path $Root 'daemon.log')"
    return
}

& $dp serve -config $cfg
