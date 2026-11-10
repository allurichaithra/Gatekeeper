$ErrorActionPreference = 'SilentlyContinue'
$root = Split-Path -Parent $PSScriptRoot
$processFile = if ($env:GATEKEEPER_PROCESS_FILE) { $env:GATEKEEPER_PROCESS_FILE } else { Join-Path $root 'data/gatekeeper-processes.json' }
if (-not (Test-Path -LiteralPath $processFile)) {
  Write-Host 'No Gatekeeper process list was found.'
  exit 0
}
foreach ($record in (Get-Content -LiteralPath $processFile -Raw | ConvertFrom-Json)) {
  $process = Get-Process -Id $record.id -ErrorAction SilentlyContinue
  if ($process -and $process.Path -eq $record.path -and $process.StartTime.ToUniversalTime().ToString('o') -eq $record.startedAt) {
    Stop-Process -Id $record.id -Force
  }
}
Remove-Item -LiteralPath $processFile -Force
Write-Host 'Stopped the Gatekeeper processes started by start-all.ps1.'

