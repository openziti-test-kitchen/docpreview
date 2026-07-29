<#
.SYNOPSIS
    Prove redaction end to end: put a real secret in the vault, have a build print it every way a careless tool
    might, then read the log back off disk.

.DESCRIPTION
    Runs its own daemon on port 8495 against its own data directory, so it does not disturb the main demo.
    Exits non-zero if the secret is found anywhere under that data directory.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [string] $Secret = 'dpfake_9f2a1c4b7e01d3f5a8c6b2e4'
)

$ErrorActionPreference = 'Stop'

$dp = & (Join-Path $PSScriptRoot 'Resolve-Docpreview.ps1') -Root $Root
$cfg  = Join-Path $Root 'secret-config.yml'
$data = Join-Path $Root 'secret-data'
$work = Join-Path $Root 'work-secrets'

$env:DOCPREVIEW_MASTER_KEY = 'flow-demo-passphrase'

foreach ($d in @($data, (Join-Path $Root 'secret-repos'), $work)) {
    if (Test-Path $d) { Remove-Item -Recurse -Force $d }
}

Set-Content -LiteralPath $cfg -Encoding utf8 -Value @"
listen: "127.0.0.1:8495"
data_dir: "$($data -replace '\\', '\\')"
workers: 1
exposer:
  kind: "local"
local:
  enabled: true
  repos_dir: "$((Join-Path $Root 'secret-repos') -replace '\\', '\\')"
  default_base: "main"
github:
  app_id: 0
build:
  driver: "local"
  timeout: 5m
  secrets:
    ALGOLIA_WRITE_KEY: "demo.algolia_key"
preview:
  ttl: 72h
"@

$Secret | & $dp vault set demo.algolia_key -config $cfg
& $dp sim init secrets -config $cfg | Out-Null

New-Item -ItemType Directory -Force -Path (Join-Path $work 'docs') | Out-Null

Set-Content -LiteralPath (Join-Path $work 'package.json') -Encoding utf8 -Value @'
{"name":"secrets","version":"0.0.0","private":true,"scripts":{"build":"node build.js"}}
'@

# Every way a build tool might spill a credential.
Set-Content -LiteralPath (Join-Path $work 'build.js') -Encoding utf8 -Value @'
const fs = require('fs');
const key = process.env.ALGOLIA_WRITE_KEY || '(unset)';

console.log('raw            ' + key);
console.log('url-encoded    ' + encodeURIComponent(key));
console.log('json           ' + JSON.stringify({apiKey: key}));
console.log('base64         ' + Buffer.from(key).toString('base64'));
console.log('in a url       https://user:' + encodeURIComponent(key) + '@algolia.net/1/indexes');
console.log('authorization  Authorization: Basic ' + Buffer.from(key).toString('base64'));
// Split across two writes with no newline between them, which is what defeats a scrubber that looks at
// each write in isolation.
process.stdout.write('split          ' + key.slice(0, 12));
process.stdout.write(key.slice(12) + '\n');

fs.rmSync('build', {recursive: true, force: true});
fs.mkdirSync('build');
fs.writeFileSync('build/index.html', '<!doctype html><meta charset=utf-8><h1>secrets</h1>');
'@

Set-Content -LiteralPath (Join-Path $work '.docpreview.yml') -Encoding utf8 -Value @'
build:
  dir: .
  command: node build.js
  output: build
  base_url: /
detect:
  paths: ["docs/**", "**/*.md"]
'@

Set-Content -LiteralPath (Join-Path $work 'docs\intro.md') -Encoding utf8 -Value '# Secrets test'

$daemon = $null
Push-Location $work
try {
    git init --quiet --initial-branch=main
    git config user.email flow@example.com
    git config user.name  'Flow Test'
    git add -A
    git commit --quiet -m 'initial'
    git remote add preview (Join-Path $Root 'secret-repos\secrets.git')
    git push --quiet preview main

    $daemon = Start-Process -PassThru -NoNewWindow -FilePath $dp `
        -ArgumentList 'serve', '-config', $cfg `
        -RedirectStandardOutput (Join-Path $Root 'secret-daemon.log') `
        -RedirectStandardError  (Join-Path $Root 'secret-daemon.err')

    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        try {
            Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 `
                -Uri 'http://127.0.0.1:8495/healthz' | Out-Null
            break
        } catch { Start-Sleep -Seconds 1 }
    }

    git checkout --quiet -b leaky main
    Add-Content -LiteralPath 'docs\intro.md' -Value "`nleak everything`n"
    git add -A
    git commit --quiet -m 'docs: try to leak the key'
    git push --quiet preview leaky

    Start-Sleep -Seconds 12
} finally {
    Pop-Location
    if ($daemon -and -not $daemon.HasExited) { Stop-Process -Id $daemon.Id -Force }
}

Write-Host ''
Write-Host '=== the build log as stored on disk ==='
Get-ChildItem -Recurse -Filter '*.log' -LiteralPath (Join-Path $data 'logs') |
    ForEach-Object { Get-Content -LiteralPath $_.FullName }

Write-Host ''
Write-Host '=== verdict ==='
$hits = Get-ChildItem -Recurse -File -LiteralPath $data |
    Select-String -SimpleMatch -Pattern $Secret -List

if ($hits) {
    Write-Host 'FAIL: the secret is somewhere under the data directory'
    $hits | ForEach-Object { Write-Host "  $($_.Path)" }
    exit 1
}

Write-Host 'PASS: the secret appears nowhere under the data directory'
