<#
.SYNOPSIS
    Push a wave of branches across every project at once, so the dashboard has genuine concurrency to show:
    several builds in flight, several queued behind them.

.EXAMPLE
    .\Send-Burst.ps1 "a fresh edit"
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)] [string] $Note = 'a fresh edit',
    [string]   $Root = $PSScriptRoot,
    [string[]] $Projects = @('mydocs', 'api-docs', 'handbook', 'design-system'),
    [string[]] $Branches = @('new-install-guide', 'api-reference', 'security-notes')
)

$ErrorActionPreference = 'Stop'

$jobs = @()

foreach ($proj in $Projects) {
    $work = Join-Path $Root "work-$proj"
    if (-not (Test-Path $work)) { continue }

    foreach ($branch in $Branches) {
        Push-Location $work
        try {
            git rev-parse --verify $branch 2>$null | Out-Null
            if ($LASTEXITCODE -ne 0) {
                git checkout --quiet -b $branch main
            } else {
                git checkout --quiet $branch
            }

            Add-Content -LiteralPath 'docs\intro.md' -Value "`n$Note ($(Get-Date -Format HH:mm:ss))`n"
            git add -A
            git commit --quiet -m "docs($proj): $Note on $branch"
        } finally { Pop-Location }

        # Backgrounded so the pushes overlap rather than queue behind each other's webhook round trip. The
        # point is a burst, not a trickle. The commits above stay serial: they share one git index per repo.
        $jobs += Start-ThreadJob -ArgumentList $work, $branch -ScriptBlock {
            param($work, $branch)
            Set-Location $work
            git push --quiet preview $branch 2>&1 | Out-Null
        }
    }
}

$jobs | Wait-Job | Receive-Job
$jobs | Remove-Job

Write-Host "pushed $($Branches.Count) branches across $($Projects.Count) projects"
