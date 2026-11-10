<#
.SYNOPSIS
    Run the offline dashboard tests in tools/dashboardtest and report which failed.

.DESCRIPTION
    Each .mjs file in tools/dashboardtest loads internal/daemon/dashboard.html into jsdom and drives it
    against fake data, so none of them needs a running daemon. Each test exits non-zero when it fails.
    This runs them one at a time, prints the output of every failure, and exits 1 when any failed.

    Three files are skipped because they are not offline tests: live.mjs is the login helper the others
    import, and rowtext.mjs and watchqueued.mjs read /status from a daemon at NFPREVIEW_URL.

    A test that does not exit within -TimeoutSeconds counts as a failure. The dashboard's one-second
    timers keep node alive, so a test that forgets process.exit hangs rather than finishing.

    Reuses tools/dashboardtest/node_modules when it is there. With -Clean it runs npm ci first.

.PARAMETER Name
    Run only the tests whose file name matches one of these wildcards, for example 'newbuild' or 'pro*'.

.PARAMETER Clean
    npm ci before running.

.PARAMETER TimeoutSeconds
    How long one test may run before it counts as hung.

.EXAMPLE
    ./Test-Dashboard.ps1

.EXAMPLE
    ./Test-Dashboard.ps1 -Name newbuild, projectedit
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [string[]] $Name = @('*'),
    [switch] $Clean,
    [int] $TimeoutSeconds = 90
)

$ErrorActionPreference = 'Stop'
$dir = Join-Path $Root 'tools/dashboardtest'
$notOffline = @('live.mjs', 'rowtext.mjs', 'watchqueued.mjs')

if ($Clean -or -not (Test-Path (Join-Path $dir 'node_modules'))) {
    Write-Host '-- npm ci' -ForegroundColor Cyan
    Push-Location $dir
    try {
        & npm ci --no-audit --no-fund
        if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
    } finally {
        Pop-Location
    }
}

$node = (Get-Command node -ErrorAction Stop).Source
$tests = Get-ChildItem -Path $dir -Filter '*.mjs' |
    Where-Object { $_.Name -notin $notOffline } |
    Where-Object { $file = $_.BaseName; $Name | Where-Object { $file -like $_ } } |
    Sort-Object Name

if (-not $tests) { throw "no tests match $($Name -join ', ')" }

$failed = @()
foreach ($t in $tests) {
    $psi = [System.Diagnostics.ProcessStartInfo]::new($node, "`"$($t.FullName)`"")
    $psi.WorkingDirectory = $dir
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.UseShellExecute = $false
    $p = [System.Diagnostics.Process]::Start($psi)
    # Read both streams asynchronously, so a test that writes a lot cannot block on a full pipe.
    $out = $p.StandardOutput.ReadToEndAsync()
    $err = $p.StandardError.ReadToEndAsync()
    $watch = [System.Diagnostics.Stopwatch]::StartNew()

    if ($p.WaitForExit($TimeoutSeconds * 1000)) {
        $p.WaitForExit()
        $result = if ($p.ExitCode -eq 0) { 'ok' } else { "FAIL (exit $($p.ExitCode))" }
    } else {
        $p.Kill($true)
        $p.WaitForExit()
        $result = "FAIL (no exit after ${TimeoutSeconds}s)"
    }
    $secs = [math]::Round($watch.Elapsed.TotalSeconds, 1)

    if ($result -eq 'ok') {
        Write-Host ("  ok    {0,-22} {1,5}s" -f $t.Name, $secs)
        continue
    }
    Write-Host ("  FAIL  {0,-22} {1,5}s  {2}" -f $t.Name, $secs, $result) -ForegroundColor Red
    $failed += [pscustomobject]@{ Name = $t.Name; Output = ($out.Result + $err.Result).TrimEnd() }
}

foreach ($f in $failed) {
    Write-Host ''
    Write-Host "-- $($f.Name)" -ForegroundColor Yellow
    Write-Host $f.Output
}

Write-Host ''
if ($failed) {
    Write-Host "$($failed.Count) of $($tests.Count) dashboard tests failed" -ForegroundColor Red
    exit 1
}
Write-Host "all $($tests.Count) dashboard tests pass" -ForegroundColor Green
