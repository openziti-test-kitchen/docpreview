<#
.SYNOPSIS
    Call a docpreview API endpoint as admin, handling the login.

.DESCRIPTION
    One place that knows how to authenticate against the daemon, because every script and every
    ad-hoc check needs the same three steps: POST /login, keep the cookie, send the real request.
    Done by hand that is a `curl` with a pasted session token in it, which goes stale on every
    restart — the signing key is generated per process — and ends up in shell history.

    Dot-source it for the function, or run it directly for one call:

        . .\Invoke-Docpreview.ps1
        Invoke-Docpreview GET /api/zrok

        .\Invoke-Docpreview.ps1 -Method POST -Path /api/zrok/exposer -Body @{ kind = 'zrok2' }

    The password comes from -Password or $env:DOCPREVIEW_PASSWORD, never from a positional
    argument, so it does not land in history by accident.

.PARAMETER Path
    The endpoint, with a leading slash.

.PARAMETER Method
    GET by default.

.PARAMETER Body
    A hashtable, sent as JSON. Ignored for GET.

.PARAMETER Raw
    Return the raw response text instead of parsed JSON.
#>
[CmdletBinding()]
param(
    [string]    $Path = '/readyz',
    [string]    $Method = 'GET',
    [hashtable] $Body,
    [string]    $Base = 'http://127.0.0.1:8471',
    [string]    $Password = $env:DOCPREVIEW_PASSWORD,
    [switch]    $Raw
)

$ErrorActionPreference = 'Stop'

# Connect-Docpreview logs in and returns a session carrying the cookie.
#
# The cookie is signed with a key the daemon generates per process, so a session does not survive a
# restart and there is nothing worth caching between runs. Logging in each time costs one request
# and removes a whole class of "why am I suddenly getting 401".
function Connect-Docpreview {
    param(
        [string] $Base = 'http://127.0.0.1:8471',
        [string] $Password = $env:DOCPREVIEW_PASSWORD
    )

    $session = New-Object Microsoft.PowerShell.Commands.WebRequestSession

    # Is a login even needed? Before any password is set the daemon serves everything, and asking
    # for one then would fail for the wrong reason.
    $probe = Invoke-WebRequest -Uri "$Base/api/admin" -WebSession $session `
        -SkipHttpErrorCheck -TimeoutSec 10
    if ($probe.StatusCode -eq 200) { return $session }

    if (-not $Password) {
        throw "the daemon wants a login: pass -Password or set `$env:DOCPREVIEW_PASSWORD"
    }

    # The redirect is followed rather than suppressed. A good login answers 303 to the dashboard,
    # and `-MaximumRedirection 0` makes PowerShell throw on it even with -SkipHttpErrorCheck — an
    # exception for the success case. Following costs one more request and keeps the cookie, which
    # is the only thing this needs.
    Invoke-WebRequest -Uri "$Base/login" -Method Post -WebSession $session `
        -Body @{ username = 'admin'; password = $Password } `
        -SkipHttpErrorCheck -TimeoutSec 10 | Out-Null

    if (-not ($session.Cookies.GetCookies($Base) | Where-Object Name -eq 'docpreview_session')) {
        throw 'the daemon refused that admin password'
    }
    return $session
}

# Invoke-Docpreview calls one endpoint, logging in first.
function Invoke-Docpreview {
    param(
        [Parameter(Position = 0)] [string] $Method = 'GET',
        [Parameter(Position = 1)] [string] $Path = '/readyz',
        [hashtable] $Body,
        [string]    $Base = 'http://127.0.0.1:8471',
        [string]    $Password = $env:DOCPREVIEW_PASSWORD,
        [switch]    $Raw
    )

    $session = Connect-Docpreview -Base $Base -Password $Password

    $args = @{
        Uri                = "$Base$Path"
        Method             = $Method
        WebSession         = $session
        SkipHttpErrorCheck = $true
        TimeoutSec         = 120
    }
    if ($Body -and $Method -ne 'GET') {
        $args['ContentType'] = 'application/json'
        $args['Body'] = ($Body | ConvertTo-Json -Compress -Depth 6)
    }

    $resp = Invoke-WebRequest @args
    if ($Raw) { return $resp.Content }

    $text = $resp.Content
    if (-not $text) { return $null }
    try { return $text | ConvertFrom-Json } catch { return $text }
}

# Run directly rather than dot-sourced: do the one call and print it.
#
# $MyInvocation.InvocationName is '.' when dot-sourced, which is the only reliable way to tell the
# two apart — and without the test, dot-sourcing would fire a stray request every time.
if ($MyInvocation.InvocationName -ne '.') {
    $result = Invoke-Docpreview -Method $Method -Path $Path -Body $Body -Base $Base `
        -Password $Password -Raw:$Raw
    if ($result -is [string]) { $result } else { $result | ConvertTo-Json -Depth 6 }
}
