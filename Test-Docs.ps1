<#
.SYNOPSIS
    Build www/ the way docpreview builds it, and report broken links.

.DESCRIPTION
    docpreview previews its own documentation, so a broken link in www/ fails a real build on a real
    pull request — which is a slow and public way to find a typo in a relative path. This is the
    same `docusaurus build`, run before pushing.

    Broken links are the failure that keeps happening, and the reason is structural: `docs/*.md` and
    `docs/runbooks/*.md` need *different* prefixes for the same target, so a link copied between
    them is wrong without looking wrong. Docusaurus resolves them at build time and refuses to
    build; nothing catches them at edit time.

    Reuses www/node_modules when it is there. With -Clean it installs first, which is what the
    container does and what catches a dependency that only works because of a stale tree.

.PARAMETER Clean
    npm ci before building.

.PARAMETER Serve
    Serve the built site afterwards, for looking at it.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [switch] $Clean,
    [switch] $Serve
)

$ErrorActionPreference = 'Stop'
$www = Join-Path $Root 'www'
Set-Location $www

if ($Clean -or -not (Test-Path (Join-Path $www 'node_modules'))) {
    Write-Host '-- npm ci' -ForegroundColor Cyan
    & npm ci --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
}

Write-Host '-- docusaurus build' -ForegroundColor Cyan
& npm run build
if ($LASTEXITCODE -ne 0) {
    Write-Host ''
    Write-Host 'The build failed. If it is a broken link, note that the prefix depends on where the' -ForegroundColor Yellow
    Write-Host 'source file lives: docs/troubleshooting.md needs ./reference/x.md, while' -ForegroundColor Yellow
    Write-Host 'docs/runbooks/y.md needs ../reference/x.md for the same target.' -ForegroundColor Yellow
    throw 'the docs build failed'
}

Write-Host ''
Write-Host 'the docs build' -ForegroundColor Green

if ($Serve) {
    Write-Host '-- serving' -ForegroundColor Cyan
    & npm run serve
}
