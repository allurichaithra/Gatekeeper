$ErrorActionPreference = 'Stop'
& (Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) 'scripts/test-integration.ps1') @args
if (-not $?) { exit 1 }
