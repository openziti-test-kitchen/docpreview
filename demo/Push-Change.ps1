<#
.SYNOPSIS
    Make a change on a branch of one project and push it. That is the whole trigger.

.EXAMPLE
    .\Push-Change.ps1 mydocs new-install-guide "rewrote the install steps"
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory, Position = 0)] [string] $Project,
    [Parameter(Mandatory, Position = 1)] [string] $Branch,
    [Parameter(Position = 2)]            [string] $Text = 'an edit',
    [string] $Root = $PSScriptRoot
)

$ErrorActionPreference = 'Stop'

Push-Location (Join-Path $Root "work-$Project")
try {
    git rev-parse --verify $Branch 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        git checkout --quiet -b $Branch main
    } else {
        git checkout --quiet $Branch
    }

    Add-Content -LiteralPath 'docs\intro.md' -Value "`n$Text`n"
    git add -A
    git commit --quiet -m "docs: $Text"
    git push --quiet preview $Branch
} finally { Pop-Location }

Write-Host "pushed $Project/$Branch"
