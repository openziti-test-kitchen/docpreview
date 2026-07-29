<#
.SYNOPSIS
    Fetch every preview URL the daemon is advertising and report which ones answer.

.DESCRIPTION
    A dashboard link that opens a connection-refused tab is worse than no link, and the only way to know which
    is which is to try them. Prints one line per preview: state, URL, verdict, branch.

    Exits non-zero if any preview in state `ready` fails to answer.
#>
[CmdletBinding()]
param(
    [string] $Base = 'http://127.0.0.1:8493'
)

$ErrorActionPreference = 'Stop'

$status = Invoke-RestMethod -Uri "$Base/status" -TimeoutSec 10
$bad = 0

foreach ($p in $status.previews) {
    if (-not $p.url) {
        $verdict = '(no url)'
    } else {
        try {
            $r = Invoke-WebRequest -Uri $p.url -TimeoutSec 5 -MaximumRedirection 0 `
                -SkipHttpErrorCheck -UseBasicParsing
            $verdict = "HTTP $($r.StatusCode)"
        } catch {
            # The message carries the useful half — refused, timed out, reset.
            $verdict = "DEAD: $($_.Exception.Message -replace '\s+', ' ')"
            if ($verdict.Length -gt 46) { $verdict = $verdict.Substring(0, 46) + '…' }
        }
    }

    if ($p.state -eq 'ready' -and $verdict -notlike 'HTTP 2*' -and $verdict -notlike 'HTTP 3*') { $bad++ }

    $colour = switch -Wildcard ($verdict) {
        'HTTP 2*'  { 'Green' }
        'HTTP 3*'  { 'Green' }
        '(no url)' { 'DarkGray' }
        default    { if ($p.state -eq 'ready') { 'Red' } else { 'DarkGray' } }
    }

    Write-Host ("{0,-9} {1,-28} " -f $p.state, ($p.url ?? '')) -NoNewline
    Write-Host ("{0,-48}" -f $verdict) -ForegroundColor $colour -NoNewline
    Write-Host " $($p.branch)"
}

Write-Host ''
if ($bad -gt 0) {
    Write-Host "$bad preview(s) in state 'ready' do not answer" -ForegroundColor Red
    exit 1
}
Write-Host 'every ready preview answers' -ForegroundColor Green
