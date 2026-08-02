<#
.SYNOPSIS
    Stand up a throwaway OpenZiti network and drive docpreview's ziti exposer against it.

.DESCRIPTION
    Everything needed to exercise the ziti path end to end on one machine, in one command, because
    doing it by hand is a dozen steps across three tools and the interesting failures are all in the
    last two.

    What it does, in order:

      1. Builds `ziti.exe` from the local checkout into build.claude, unless it is already there.
         Built rather than downloaded so it matches the SDK version docpreview is compiled against —
         the enrolment exchange is the thing under test, and a version skew there would be testing
         the wrong pair.
      2. Starts `ziti edge quickstart` in the background: a controller and a router in a temporary
         home under .docpreview\ziti-test, with its output on disk.
      3. Waits for the controller, then logs in.
      4. Creates docpreview's identity and leaves its **one-time enrolment JWT** on disk, plus the
         service, the configs and the policies the exposer needs.
      5. With -Enroll, posts that JWT to the running daemon's /api/zrok/ziti/enroll — which is the
         path this whole script exists to exercise. The daemon generates the key pair, exchanges the
         token and writes the identity beside the vault.

    None of it touches the live zrok setup: a different exposer, a different directory, and the
    daemon only changes exposer when told to.

.PARAMETER Enroll
    After the network is up, enrol the identity through the daemon's API. Needs the daemon running
    and -Password, or DOCPREVIEW_PASSWORD in the environment.

.PARAMETER Password
    The docpreview admin password, for -Enroll. Defaults to $env:DOCPREVIEW_PASSWORD.

.PARAMETER Down
    Stop the quickstart. The home directory survives, so -Down then a plain run resumes the same
    network — the enrolment token does not, since it is one-time and by then it is spent.

.PARAMETER Clean
    Stop it and delete the home directory, the issued JWT, and the identity docpreview enrolled.
    The next run is a fresh network with a fresh token, which is what you want after a spent one.

.PARAMETER Rebuild
    Rebuild ziti.exe even if it is already in build.claude.

.EXAMPLE
    .\Start-ZitiTrial.ps1
    Brings the network up and prints the enrolment token's path.

.EXAMPLE
    .\Start-ZitiTrial.ps1 -Enroll
    The same, then enrols the identity through the daemon and reports where it landed.

.EXAMPLE
    .\Start-ZitiTrial.ps1 -Clean
    Tears the whole thing down, including the enrolled identity.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [string] $ZitiSource = 'D:\git\github\openziti\ziti',
    [string] $Base = 'http://127.0.0.1:8471',
    [string] $Password = $env:DOCPREVIEW_PASSWORD,
    [string] $Identity = 'docpreview',
    [string] $Service = 'docpreview-previews',
    [string] $Domain = 'preview.ziti',
    [int]    $CtrlPort = 1280,
    [switch] $Enroll,
    [switch] $Down,
    [switch] $Clean,
    [switch] $Rebuild
)

$ErrorActionPreference = 'Stop'
Set-Location $Root

$home_    = Join-Path $Root '.docpreview\ziti-test'
$ziti     = Join-Path $Root 'build.claude\ziti.exe'
$jwt      = Join-Path $home_ "$Identity.jwt"
$ctrl     = "127.0.0.1:$CtrlPort"
$idFile   = Join-Path $Root ".docpreview\data\ziti\$Identity.json"

# ASCII, not a box-drawing rule: some consoles render em-dashes as mojibake, which makes a step
# line look like a bug.
function Write-Step([string] $Text) { Write-Host "-- $Text" -ForegroundColor Cyan }

# ── Stopping ──────────────────────────────────────────────────────────────────────────────────────
# Matched on the command line rather than on the process name, because the name is `ziti` and this
# script must not kill a tunneler or a CLI somebody else is running.
function Stop-Quickstart {
    $procs = Get-CimInstance Win32_Process -Filter "Name = 'ziti.exe'" |
        Where-Object { $_.CommandLine -match 'quickstart' -and $_.CommandLine -match [regex]::Escape($home_) }
    if (-not $procs) {
        Write-Host 'no quickstart of this trial is running'
        return
    }
    foreach ($p in $procs) {
        Write-Host "stopping quickstart PID $($p.ProcessId)"
        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Seconds 2
}

if ($Down -or $Clean) {
    Stop-Quickstart
    if ($Clean) {
        foreach ($path in @($home_, $idFile)) {
            if (Test-Path $path) {
                Write-Host "removing $path"
                Remove-Item -LiteralPath $path -Recurse -Force
            }
        }
        Write-Host 'the next run gets a fresh network and a fresh enrolment token'
    }
    return
}

# ── 1. The CLI ────────────────────────────────────────────────────────────────────────────────────
if ($Rebuild -or -not (Test-Path $ziti)) {
    Write-Step "building the ziti CLI from $ZitiSource"
    Push-Location $ZitiSource
    try {
        & go build -o $ziti .\ziti
        if ($LASTEXITCODE -ne 0) { throw 'building the ziti CLI failed' }
    } finally { Pop-Location }
}
Write-Host "ziti CLI: $ziti"

# ── 2. The network ────────────────────────────────────────────────────────────────────────────────
New-Item -ItemType Directory -Force -Path $home_ | Out-Null

# Something already on the port is the common case, not the exceptional one: a quickstart from an
# earlier session, possibly a different home and a different `ziti` binary. Reused when it is
# recognisably a quickstart, and refused otherwise — creating identities, services and policies on
# a controller somebody else is using is the kind of blast radius that is nobody's intention.
$holder = Get-NetTCPConnection -LocalPort $CtrlPort -State Listen -ErrorAction SilentlyContinue |
    Select-Object -First 1
if ($holder) {
    $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($holder.OwningProcess)" `
        -ErrorAction SilentlyContinue
    if ($proc -and $proc.CommandLine -match 'quickstart') {
        Write-Host "reusing the quickstart already on $ctrl (PID $($holder.OwningProcess))"
        if ($proc.CommandLine -notmatch [regex]::Escape($home_)) {
            # Named, because every object this script creates lands there rather than in the home
            # under .docpreview, and -Clean will not remove it.
            Write-Host '  its home is not this trial''s:' -ForegroundColor Yellow
            Write-Host "  $($proc.CommandLine)" -ForegroundColor DarkGray
        }
    } else {
        throw "port $CtrlPort is held by PID $($holder.OwningProcess), which is not a ziti " +
              "quickstart. Pass -CtrlPort with a free port rather than provisioning objects on " +
              "whatever that is."
    }
} else {
    Write-Step 'starting the quickstart controller and router'
    $out = Join-Path $home_ 'quickstart.out.log'
    $err = Join-Path $home_ 'quickstart.err.log'
    $p = Start-Process -PassThru -WindowStyle Hidden -FilePath $ziti `
        -ArgumentList @('edge', 'quickstart',
            '--home', $home_,
            '--ctrl-address', '127.0.0.1', '--ctrl-port', $CtrlPort,
            '--router-address', '127.0.0.1',
            '-u', 'admin', '-p', 'admin') `
        -RedirectStandardOutput $out -RedirectStandardError $err
    Write-Host "  PID $($p.Id)  ->  $err"

    # A controller takes a few seconds to issue its PKI and open the edge API. Polled rather than
    # slept, because the wait is the part that varies.
    Write-Host -NoNewline '  waiting for the controller'
    $deadline = (Get-Date).AddSeconds(120)
    $up = $false
    while ((Get-Date) -lt $deadline) {
        if ($p.HasExited) {
            Write-Host ''
            Get-Content $err -Tail 20 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
            throw 'the quickstart exited while starting'
        }
        try {
            Invoke-WebRequest -Uri "https://$ctrl/edge/client/v1/version" -SkipCertificateCheck `
                -TimeoutSec 3 | Out-Null
            $up = $true
            break
        } catch {
            Write-Host -NoNewline '.'
            Start-Sleep -Seconds 2
        }
    }
    Write-Host ''
    if (-not $up) { throw "the controller did not answer on $ctrl within 120s" }
}

# ── 3. Log in ─────────────────────────────────────────────────────────────────────────────────────
Write-Step 'logging in to the controller'
& $ziti edge login $ctrl -u admin -p admin -y | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'ziti edge login failed' }

# ── 4. The objects docpreview needs ───────────────────────────────────────────────────────────────
# Mirrors `docpreview configure ziti`, minus the part under test: the identity is created here and
# left *unenrolled*, so its one-time JWT is what the daemon consumes. Provisioning it with the CLI
# would enrol it too and there would be nothing left to test.
#
# `ziti edge create` is not idempotent — a second create answers 409 — so each object is checked
# first. That makes the script re-runnable, which matters because the interesting failures happen at
# step 5 and re-running everything from scratch to retry one call is how a trial stops being used.
# Listed and matched here rather than filtered by the controller: the filter form is `name="x"`,
# whose quotes have to survive PowerShell *and* the CLI's own parser, and if they do not, the filter
# matches nothing, every object looks absent, and a second run fails on a duplicate name instead of
# skipping. Listing everything of one kind on a trial network is a handful of rows.
function Test-ZitiObject([string] $Kind, [string] $Name) {
    $json = & $ziti edge list $Kind --output-json 2>$null
    if ($LASTEXITCODE -ne 0 -or -not $json) { return $false }
    try { $parsed = $json | ConvertFrom-Json } catch { return $false }
    return $null -ne ($parsed.data | Where-Object { $_.name -eq $Name })
}

if (Test-ZitiObject 'identities' $Identity) {
    Write-Host "identity $Identity exists"
} else {
    Write-Step "creating the identity $Identity"
    & $ziti edge create identity $Identity -a $Identity -o $jwt
    if ($LASTEXITCODE -ne 0) { throw "creating the identity failed" }
    Write-Host "  enrolment token: $jwt"
}

# The two configs and the service. Addresses are the wildcard the exposer serves previews under, so
# they must match exposer.ziti.domain or the tunneler resolves names the daemon does not answer to —
# the mismatch is invisible until somebody opens a link.
$interceptCfg = "$Service-intercept"
$hostCfg      = "$Service-host"

if (-not (Test-ZitiObject 'configs' $interceptCfg)) {
    Write-Step "creating $interceptCfg"
    $json = @{
        protocols = @('tcp')
        addresses = @("*.$Domain")
        portRanges = @(@{ low = 80; high = 80 })
    } | ConvertTo-Json -Compress -Depth 5
    & $ziti edge create config $interceptCfg intercept.v1 $json
    if ($LASTEXITCODE -ne 0) { throw "creating $interceptCfg failed" }
}

if (-not (Test-ZitiObject 'configs' $hostCfg)) {
    Write-Step "creating $hostCfg"
    # forwardPort/forwardAddress, because docpreview binds the service itself through the SDK rather
    # than being fronted by a tunneler — the host config exists so the service is well-formed.
    $json = @{ protocol = 'tcp'; address = '127.0.0.1'; port = 8471 } | ConvertTo-Json -Compress
    & $ziti edge create config $hostCfg host.v1 $json
    if ($LASTEXITCODE -ne 0) { throw "creating $hostCfg failed" }
}

if (-not (Test-ZitiObject 'services' $Service)) {
    Write-Step "creating the service $Service"
    # One comma-joined string, not two arguments. `--configs a, b` in PowerShell passes `a,` and
    # `b` as separate argv entries, and the CLI takes exactly one positional — so it rejected the
    # second as a stray argument and the error was about arg counts rather than about configs.
    & $ziti edge create service $Service -a $Service --configs "$interceptCfg,$hostCfg"
    if ($LASTEXITCODE -ne 0) { throw "creating the service failed" }
}

# Bind for docpreview, Dial for anything holding the reader attribute. Exactly one docpreview may
# bind: two binders means two terminators and roughly half of all previews answering 404 while every
# log looks healthy.
if (-not (Test-ZitiObject 'service-policies' "$Service-bind")) {
    Write-Step 'creating the bind policy'
    & $ziti edge create service-policy "$Service-bind" Bind --service-roles "@$Service" --identity-roles "#$Identity"
    if ($LASTEXITCODE -ne 0) { throw 'creating the bind policy failed' }
}
if (-not (Test-ZitiObject 'service-policies' "$Service-dial")) {
    Write-Step 'creating the dial policy'
    & $ziti edge create service-policy "$Service-dial" Dial --service-roles "@$Service" --identity-roles '#all'
    if ($LASTEXITCODE -ne 0) { throw 'creating the dial policy failed' }
}

Write-Host ''
Write-Host "controller : https://$ctrl   (admin/admin)"
Write-Host "service    : $Service"
Write-Host "domain     : *.$Domain"
Write-Host "home       : $home_"
if (Test-Path $jwt) { Write-Host "token      : $jwt" }

# ── 5. Enrol through the daemon ───────────────────────────────────────────────────────────────────
if (-not $Enroll) {
    Write-Host ''
    Write-Host 'Enrol it through the daemon with:  .\Start-ZitiTrial.ps1 -Enroll'
    return
}

if (-not (Test-Path $jwt)) {
    throw "no enrolment token at $jwt — it is one-time and this one is spent. " +
          "Run with -Clean and start again."
}
if (-not $Password) {
    throw 'no admin password: pass -Password or set $env:DOCPREVIEW_PASSWORD'
}

Write-Step 'enrolling the identity through the daemon'
. (Join-Path $Root 'Invoke-Docpreview.ps1')

$resp = Invoke-Docpreview POST /api/zrok/ziti/enroll -Base $Base -Password $Password -Body @{
    jwt     = (Get-Content -LiteralPath $jwt -Raw).Trim()
    service = $Service
    domain  = $Domain
}
if ($resp.error) { throw "the daemon refused the enrolment: $($resp.error)" }

Write-Host $resp.note -ForegroundColor Green

# The token is consumed whether or not anything else went right, so it goes now. Leaving a spent
# JWT on disk is leaving a file that looks usable and answers 404 — and the script's own check for
# "is there a token" would then pass on the next run and fail at the controller instead.
Remove-Item -LiteralPath $jwt -Force -ErrorAction SilentlyContinue

Write-Host ''
Write-Host 'The token is spent. To publish through it:'
Write-Host '  .\Set-Exposer.ps1 -Base http://127.0.0.1:8493 -Kind ziti -Restart'
Write-Host ''
Write-Host 'The demo daemon, not the live one. Switching the instance on :8471 rewrites the comment'
Write-Host 'on every open pull request, twice — once going and once coming back — and that is'
Write-Host 'somebody else''s notifications. Set-Exposer.ps1 refuses :8471 without -Live.'
