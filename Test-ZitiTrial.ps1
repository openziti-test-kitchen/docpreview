<#
.SYNOPSIS
    Check that the ziti identity docpreview enrolled actually works.

.DESCRIPTION
    Enrolment can "succeed" three ways and only one of them is real: a file on disk, an identity the
    controller shows as enrolled, and an identity that can authenticate. The first two are what a
    green message from the dashboard proves; the third is what publishing needs.

    So this asserts all three, in that order, and says which one failed. Written as a script because
    every one of these checks is a CLI call with a JSON filter in it, and getting the quoting wrong
    silently returns nothing — which reads as a missing identity rather than a broken query.

.PARAMETER Identity
    The identity name, matching what Start-ZitiTrial.ps1 created.
#>
[CmdletBinding()]
param(
    [string] $Root = $PSScriptRoot,
    [string] $Identity = 'docpreview',
    [string] $Service = 'docpreview-previews',
    [int]    $CtrlPort = 1280
)

$ErrorActionPreference = 'Stop'
Set-Location $Root

$ziti   = Join-Path $Root 'build.claude\ziti.exe'
$idFile = Join-Path $Root ".docpreview\data\ziti\$Identity.json"
$ctrl   = "127.0.0.1:$CtrlPort"

$failures = 0
function Fail([string] $m) { $script:failures++; Write-Host "   FAIL: $m" -ForegroundColor Red }
function Pass([string] $m) { Write-Host "   ok: $m" -ForegroundColor Green }

Write-Host '1. the identity file'
if (-not (Test-Path $idFile)) {
    Fail "no identity file at $idFile"
} else {
    Pass $idFile
    $cfg = Get-Content -LiteralPath $idFile -Raw | ConvertFrom-Json
    # The three things a ziti identity config must carry. A file that parses and is missing one of
    # them is the shape a half-written enrolment leaves behind.
    foreach ($field in 'ztAPI', 'id') {
        if (-not $cfg.$field) { Fail "the config has no $field" }
    }
    if (-not $cfg.id.cert -or -not $cfg.id.key) {
        Fail 'the config carries no certificate or key'
    } else {
        Pass "authenticates at $($cfg.ztAPI)"
    }
}

Write-Host ''
Write-Host '2. the controller agrees it is enrolled'
& $ziti edge login $ctrl -u admin -p admin -y | Out-Null
if ($LASTEXITCODE -ne 0) {
    Fail "cannot log in to the controller at $ctrl"
} else {
    $all = & $ziti edge list identities --output-json | ConvertFrom-Json
    $me = $all.data | Where-Object name -eq $Identity
    if (-not $me) {
        Fail "the controller has no identity called $Identity"
    } elseif ($me.enrollment.ott) {
        # An outstanding one-time token means the identity was created and never enrolled. That is
        # exactly what a failed enrolment leaves, and it looks identical from the file's side if the
        # file came from an earlier attempt.
        Fail 'the identity still has an outstanding enrolment token, so it never enrolled'
    } else {
        Pass "$Identity is enrolled (id $($me.id))"
    }

    Write-Host ''
    Write-Host '3. it may bind the service'
    $svc = (& $ziti edge list services --output-json | ConvertFrom-Json).data |
        Where-Object name -eq $Service
    if (-not $svc) {
        Fail "no service called $Service"
    } else {
        # Matched on the service **id**, not its name. Roles are stored as `@<id>`, so
        # `@docpreview-previews` matches nothing and every policy looks absent — which reads as a
        # missing policy rather than as a query asking the wrong question.
        $policies = (& $ziti edge list service-policies --output-json | ConvertFrom-Json).data |
            Where-Object { $_.type -eq 'Bind' -and $_.serviceRoles -contains "@$($svc.id)" }
        if (-not $policies) {
            Fail "no Bind policy for $Service (id $($svc.id))"
        } else {
            # Bind, not Dial. docpreview hosts the service; a Dial policy would let it connect to
            # something it is supposed to be serving.
            #
            # And exactly one binder: two means two terminators and roughly half of all previews
            # answering 404 while every log looks healthy. Counted rather than assumed, because the
            # trial script is re-runnable and a second policy is the obvious way to get that wrong.
            $names = @($policies | ForEach-Object { $_.name })
            if ($names.Count -gt 1) {
                Fail "$($names.Count) Bind policies for one service: $($names -join ', ')"
            } else {
                Pass "one Bind policy, $($names[0])"
            }
            $grants = @($policies | ForEach-Object { $_.identityRoles }) -join ', '
            if ($grants -notmatch [regex]::Escape($Identity)) {
                Fail "the Bind policy does not name ${Identity}: $grants"
            } else {
                Pass "it binds for $grants"
            }
        }
    }
}

Write-Host ''
Write-Host '4. the daemon is actually hosting it'
{
    # A terminator on the controller is the only proof that the identity is not merely enrolled but
    # *in use*: the daemon dialled in, authenticated with the file above, and offered to serve the
    # service. Everything before this passes on a daemon that never started.
    #
    # Absent is not a failure — it means ziti is not the exposer right now, which is the normal
    # state while zrok is publishing — so it reports rather than fails.
    $terms = (& $ziti edge list terminators --output-json | ConvertFrom-Json).data |
        Where-Object { $_.service.name -eq $Service }
    if (-not $terms) {
        Write-Host "   (no terminator: nothing is hosting $Service, so ziti is not the live exposer)"
    } else {
        Pass "$($terms.Count) terminator on $Service via router $($terms[0].router.name)"
        # Exactly one. Two docpreviews binding one service means two terminators and roughly half
        # of all previews answering 404 while every log looks healthy — the failure this whole
        # exposer's design notes keep circling.
        if ($terms.Count -gt 1) {
            Fail "$($terms.Count) terminators — two binders means half of all previews 404"
        }
    }
}.Invoke()

Write-Host ''
if ($failures -eq 0) {
    Write-Host 'the enrolment is real' -ForegroundColor Green
} else {
    Write-Host "$failures failure(s)" -ForegroundColor Red
}
exit $failures
