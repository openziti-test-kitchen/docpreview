<#
.SYNOPSIS
    Build the Docusaurus template every demo project is cut from.

.DESCRIPTION
    Scaffolding once and copying is deliberate: create-docusaurus resolves the whole dependency graph from the
    network, and doing that four times produces four subtly different lockfiles for what is meant to be one
    control variable.

    Safe to re-run: it does nothing if the template is already installed. Use -Force to rebuild it.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [switch] $Force
)

$ErrorActionPreference = 'Stop'

$template = Join-Path $Root 'template'

if ((Test-Path (Join-Path $template 'node_modules')) -and -not $Force) {
    Write-Host "template already built at $template"
    return
}

if (Test-Path $template) { Remove-Item -Recurse -Force $template }
New-Item -ItemType Directory -Force -Path $Root | Out-Null

Push-Location $Root
try {
    Write-Host '== scaffolding docusaurus (this pulls the dependency graph)'
    npx --yes create-docusaurus@latest template classic --typescript --package-manager yarn
    if ($LASTEXITCODE -ne 0) { throw "create-docusaurus failed with exit code $LASTEXITCODE" }

    Write-Host '== warming the yarn cache'
    Push-Location $template
    try {
        yarn install --frozen-lockfile --non-interactive
        if ($LASTEXITCODE -ne 0) { throw "yarn install failed with exit code $LASTEXITCODE" }
    } finally { Pop-Location }
} finally { Pop-Location }

Write-Host ''
Write-Host "template ready: $template"
