<#
.SYNOPSIS
    Keep the dashboard alive: a wave of pushes every so often, so there is always something building whenever
    somebody looks.

.DESCRIPTION
    Runs until you stop it with Ctrl-C.

.EXAMPLE
    .\Start-KeepBusy.ps1
    .\Start-KeepBusy.ps1 -IntervalSeconds 120 -Waves 3
#>
[CmdletBinding()]
param(
    # A real Docusaurus build is a yarn install plus a webpack run — twenty-odd seconds with a warm yarn
    # cache, minutes on the first cold one. Waves closer together than a build supersede each other before
    # anything finishes, so the dashboard shows nothing but queued.
    [int] $IntervalSeconds = 90,

    # 0 means forever.
    [int] $Waves = 0,

    [string]   $Root = $PSScriptRoot,
    [string[]] $Projects = @('mydocs', 'api-docs', 'handbook', 'design-system'),
    [string[]] $Branches = @('new-install-guide', 'api-reference', 'security-notes', 'broken-build')
)

$ErrorActionPreference = 'Stop'

# A real Docusaurus failure, not a scripted one: an MDX file importing a component that does not exist fails
# during the client bundle, which is far enough into the build that the log has genuine output above the
# error — which is the whole point of having a failing branch to look at.
$brokenDoc = @'
---
title: Broken on purpose
---

import Missing from '@site/src/components/DoesNotExist';

# Broken on purpose

<Missing />
'@

$jobs = @()
$wave = 1

try {
    while ($true) {
        Write-Host "== wave $wave at $(Get-Date -Format HH:mm:ss)"

        foreach ($proj in $Projects) {
            $work = Join-Path $Root "work-$proj"
            if (-not (Test-Path $work)) { continue }

            # A different branch per project per wave, so the mix changes rather than the same four cards
            # cycling.
            $branch = $Branches[($wave + $proj.Length) % $Branches.Count]

            # broken-build only exists on api-docs, so the failure is always in the same place to look for.
            if ($branch -eq 'broken-build' -and $proj -ne 'api-docs') { $branch = $Branches[0] }

            $jobs += Start-ThreadJob -ArgumentList $work, $branch, $wave, $brokenDoc -ScriptBlock {
                param($work, $branch, $wave, $brokenDoc)

                Set-Location $work

                git rev-parse --verify $branch 2>$null | Out-Null
                if ($LASTEXITCODE -ne 0) {
                    git checkout --quiet -b $branch main
                } else {
                    git checkout --quiet $branch
                }

                if ($branch -eq 'broken-build') {
                    Set-Content -LiteralPath 'docs\broken.mdx' -Value $brokenDoc -Encoding utf8
                }

                Add-Content -LiteralPath 'docs\intro.md' -Value "`nwave $wave at $(Get-Date -Format HH:mm:ss)`n"
                git add -A
                git commit --quiet -m "docs: wave $wave on $branch"
                git push --quiet preview $branch 2>&1 | Out-Null
            }
        }

        $jobs | Wait-Job | Receive-Job
        $jobs | Remove-Job
        $jobs = @()

        if ($Waves -gt 0 -and $wave -ge $Waves) { break }
        $wave++
        Start-Sleep -Seconds $IntervalSeconds
    }
} finally {
    # Ctrl-C leaves the wave's threads running otherwise, and they hold the git index of every working copy.
    $jobs | Remove-Job -Force -ErrorAction SilentlyContinue
}
