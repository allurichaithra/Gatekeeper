function Get-GatekeeperAccessToken([string]$ControlApiUrl = 'http://localhost:8081') {
  if ($env:GATEKEEPER_TEST_TOKEN) {
    try {
      $session = Invoke-RestMethod -Method Get -Uri ($ControlApiUrl.TrimEnd('/') + '/api/auth/me') -Headers @{ Authorization = "Bearer $env:GATEKEEPER_TEST_TOKEN" }
      if ($session.role -eq 'ADMIN') { return [string]$env:GATEKEEPER_TEST_TOKEN }
    } catch { }
  }
  $username = $env:GATEKEEPER_TEST_USERNAME
  $password = $env:GATEKEEPER_TEST_PASSWORD
  $securePassword = $null
  $passwordPointer = [IntPtr]::Zero
  if (-not $username) { $username = Read-Host 'Gatekeeper administrator username' }
  if (-not $password) {
    $securePassword = Read-Host 'Gatekeeper administrator password' -AsSecureString
    $passwordPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($securePassword)
    $password = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($passwordPointer)
  }
  try {
    $response = Invoke-RestMethod -Method Post -Uri ($ControlApiUrl.TrimEnd('/') + '/api/auth/login') -ContentType 'application/json' -Body (@{ username = $username; password = $password } | ConvertTo-Json -Compress)
    if (-not $response.accessToken) { throw 'Login response did not contain an access token.' }
    $token = [string]$response.accessToken
    $session = Invoke-RestMethod -Method Get -Uri ($ControlApiUrl.TrimEnd('/') + '/api/auth/me') -Headers @{ Authorization = "Bearer $token" }
    if ($session.role -ne 'ADMIN') { throw 'Load and integration test scripts require an administrator account.' }
    return $token
  } finally {
    if ($passwordPointer -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($passwordPointer) }
    if ($securePassword) { $securePassword.Dispose() }
    $password = $null
  }
}
