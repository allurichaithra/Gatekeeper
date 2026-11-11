$ErrorActionPreference = 'Stop'
$scriptPath = Join-Path $PSScriptRoot 'test-integration.ps1'
Write-Host 'Gatekeeper isolated end-to-end demonstration'
Write-Host 'The demo uses temporary ports, a temporary SQLite file and throwaway test credentials.'
try {
  & $scriptPath
  if (-not $?) { throw 'Integration suite reported a failure.' }
  Write-Host 'DEMO RESULT: PASS - isolated integration demonstration completed.' -ForegroundColor Green
  exit 0
} catch {
  Write-Error ('DEMO RESULT: FAIL - ' + $_.Exception.Message)
  exit 1
}
