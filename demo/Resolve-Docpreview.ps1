<#
.SYNOPSIS
    Return the path to the docpreview binary the harness should run.

.DESCRIPTION
    Dot-sourced by the other scripts. `go build` in this repo writes to build.claude\, so that is the first
    place to look — running the harness against a stale copy sitting in demo\ is the sort of thing that costs
    an hour before anyone notices the fix they just made is not in the binary.
#>
[CmdletBinding()]
param([string] $Root = $PSScriptRoot)

$built = Join-Path (Split-Path -Parent $Root) 'build.claude\docpreview.exe'
$local = Join-Path $Root 'docpreview.exe'

if (Test-Path $built) { return (Resolve-Path $built).Path }
if (Test-Path $local) { return (Resolve-Path $local).Path }

throw "no docpreview.exe — build one with: go build -o build.claude\ .\cmd\docpreview\"
