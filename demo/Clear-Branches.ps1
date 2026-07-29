<#
.SYNOPSIS
    Delete the feature branches, locally and on the preview remote, so the next wave cuts them from current main.

.DESCRIPTION
    A branch cut before main changed carries the old .docpreview.yml and builds the old way regardless of what
    main now says — which is exactly how a stale branch behaves in real life, and exactly what makes the
    dashboard flick from queued to ready with nothing visible between.
#>
[CmdletBinding()]
param(
    [string]   $Root = $PSScriptRoot,
    [string[]] $Projects = @('mydocs', 'api-docs', 'handbook', 'design-system'),
    [string[]] $Branches = @('new-install-guide', 'api-reference', 'security-notes', 'broken-build')
)

$ErrorActionPreference = 'Stop'

foreach ($proj in $Projects) {
    $work = Join-Path $Root "work-$proj"
    if (-not (Test-Path $work)) { continue }

    Push-Location $work
    try {
        git checkout --quiet main

        foreach ($branch in $Branches) {
            # Both sides are best-effort: a branch that was never cut on this project is not an error.
            git branch -D $branch 2>&1 | Out-Null
            git push --quiet preview --delete $branch 2>&1 | Out-Null
        }
    } finally { Pop-Location }
}

$global:LASTEXITCODE = 0
Write-Host 'feature branches cleared; the next wave cuts them from current main'
