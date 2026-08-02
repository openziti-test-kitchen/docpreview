<#
.SYNOPSIS
    Stand up several independent "projects" so the dashboard has real concurrency to show.

.DESCRIPTION
    Each project is its own bare repo, its own working copy, its own branches — and a real Docusaurus site
    built with `yarn build`.

    A genuine Docusaurus build emits a few hundred lines over a minute and a half, which is what the live log
    tail is for: a stand-in build that finishes in five lines gives nothing to stream, so there is no way to
    tell a working stream from a broken one.

    Destructive: it recreates every bare repo and every working copy from scratch.
#>
[CmdletBinding()]
param(
    [string]   $Root = $PSScriptRoot,
    [string[]] $Projects = @('mydocs', 'api-docs', 'handbook', 'design-system')
)

$ErrorActionPreference = 'Stop'

$dp = & (Join-Path $PSScriptRoot 'Resolve-Docpreview.ps1') -Root $Root
$cfg      = Join-Path $Root 'config.yml'
$template = Join-Path $Root 'template'

if (-not (Test-Path (Join-Path $template 'node_modules'))) {
    throw "no template at $template — run .\Build-Template.ps1 first"
}

# See Start-Demo.ps1 for why a fixed passphrase is acceptable here.
if (-not $env:DOCPREVIEW_MASTER_KEY) { $env:DOCPREVIEW_MASTER_KEY = 'flow-demo-passphrase' }

# config.yml maps ALGOLIA_WRITE_KEY to this vault entry. Storing it here rather than in the config file is
# the whole point: the value never appears in anything tracked, and the redactor is built from the value.
'dpfake_9f2a1c4b7e01d3f5a8c6b2e4' | & $dp vault set demo.algolia_key -config $cfg
if ($LASTEXITCODE -ne 0) { throw "storing the demo secret failed with exit code $LASTEXITCODE" }

# baseUrl has to come from the environment, because docpreview mounts each preview under a path it chooses and
# Docusaurus bakes the value in at build time. DOCUSAURUS_BASE_URL is set by the builder for exactly this.
#
# Only the first `title:` is rewritten: the same key appears on the navbar and on every footer column, and
# rewriting those would rename the links rather than the site.
function Set-PreviewableConfig {
    param([Parameter(Mandatory)] [string] $Path)

    $text = Get-Content -Raw -LiteralPath $Path

    $text = [regex]::Replace($text, "baseUrl: '[^']*',",
        "baseUrl: process.env.DOCUSAURUS_BASE_URL || '/',", 1)
    $text = [regex]::Replace($text, "title: '[^']*',",
        "title: process.env.PROJECT_NAME || 'Docs',", 1)
    # The broken-link check aborts the build on the template's own placeholder links once baseUrl moves. Warn
    # instead: this is a preview, not a release.
    $text = [regex]::Replace($text, "onBrokenLinks: '[^']*',", "onBrokenLinks: 'warn',", 1)

    Set-Content -LiteralPath $Path -Value $text -NoNewline -Encoding utf8
}

foreach ($proj in $Projects) {
    $repo = Join-Path $Root "repos\$proj.git"
    $work = Join-Path $Root "work-$proj"

    # The remote is recreated from scratch. Reseeding on top of an existing one pushes a fresh history at a ref
    # that already has a different one, which git rejects — and keeping the old history would leave stale
    # previews pointing at commits that no longer describe the project.
    Write-Host "== creating remote $proj"
    if (Test-Path $repo) { Remove-Item -Recurse -Force $repo }
    & $dp sim init $proj -config $cfg | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "docpreview sim init $proj failed with exit code $LASTEXITCODE" }

    Write-Host "== seeding $proj"
    if (Test-Path $work) { Remove-Item -Recurse -Force $work }
    New-Item -ItemType Directory -Force -Path $work | Out-Null

    # Everything but the installed tree and previous output. node_modules is rebuilt per build on purpose: that
    # install is most of the log, and skipping it would hide the part of a real build that most often fails.
    Get-ChildItem -LiteralPath $template -Force |
        Where-Object { $_.Name -notin @('node_modules', 'build', '.docusaurus', '.git') } |
        ForEach-Object { Copy-Item -Recurse -Force -LiteralPath $_.FullName -Destination $work }

    Set-PreviewableConfig (Join-Path $work 'docusaurus.config.ts')

    # A build step that spills its own configuration, which is what a careless tool does and what redaction
    # exists to survive. yarn runs `prebuild` ahead of `build` with no wiring, so this lands in the live log
    # above the Docusaurus output on every single build.
    Set-Content -LiteralPath (Join-Path $work 'preflight.js') -Encoding utf8 -Value @'
// Deliberately indiscreet. Every line below reaches the build log, and every
// occurrence of ALGOLIA_WRITE_KEY in it should arrive as five asterisks —
// including the base64 and URL-encoded forms, which a naive scrubber misses.
const key = process.env.ALGOLIA_WRITE_KEY || '(no secret configured)';

console.log('preflight: project      ' + (process.env.PROJECT_NAME || '?'));
console.log('preflight: base url     ' + (process.env.DOCUSAURUS_BASE_URL || '/'));
console.log('preflight: search key   ' + key);
console.log('preflight: index url    https://user:' + encodeURIComponent(key) + '@algolia.net/1/indexes');
console.log('preflight: auth header  Authorization: Basic ' + Buffer.from(key).toString('base64'));
console.log('preflight: ok');
'@

    $pkgPath = Join-Path $work 'package.json'
    $pkg = Get-Content -Raw -LiteralPath $pkgPath | ConvertFrom-Json
    $pkg.scripts | Add-Member -NotePropertyName 'prebuild' -NotePropertyValue 'node preflight.js' -Force
    $pkg | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $pkgPath -Encoding utf8

    Set-Content -LiteralPath (Join-Path $work '.docpreview.yml') -Encoding utf8 -Value @"
build:
  dir: .
  command: yarn build
  output: build
  base_url: /
  env:
    PROJECT_NAME: "$proj"
detect:
  paths:
    - "docs/**"
    - "blog/**"
    - "**/*.md"
    - "**/*.mdx"
"@

    Set-Content -LiteralPath (Join-Path $work '.gitignore') -Encoding utf8 -Value @'
node_modules/
build/
.docusaurus/
.yarn/cache
'@

    # The template ships docs/intro.mdx. Two files resolving to the same doc id is a hard build error, and the
    # one written below is the one Push-Change.ps1 edits.
    Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $work 'docs\intro.mdx')

    Set-Content -LiteralPath (Join-Path $work 'docs\intro.md') -Encoding utf8 -Value @"
---
sidebar_position: 1
---

# $proj

The introduction page for **$proj**.

This site is a real Docusaurus build, produced by ``yarn build`` inside a docpreview workspace. Editing
anything under ``docs/`` and pushing rebuilds it.
"@

    Push-Location $work
    try {
        git init --quiet --initial-branch=main
        git config user.email flow@example.com
        git config user.name  'Flow Test'
        git add -A
        git commit --quiet -m "initial docs for $proj"
        git remote add preview $repo
        git push --quiet preview main
    } finally { Pop-Location }
}

Write-Host ''
Write-Host "ready: $($Projects -join ' ')"
