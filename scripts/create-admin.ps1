$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$username = Read-Host 'Initial administrator username'
$password = Read-Host 'Initial administrator password (12–128 characters)' -AsSecureString
$confirm = Read-Host 'Confirm administrator password' -AsSecureString
$passwordPtr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($password)
$confirmPtr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($confirm)
try {
  $plainPassword = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($passwordPtr)
  $confirmPassword = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($confirmPtr)
  if ($plainPassword -cne $confirmPassword) { throw 'Passwords do not match.' }
  $env:SETUP_ADMIN_USERNAME = $username
  $env:SETUP_ADMIN_PASSWORD = $plainPassword
  node (Join-Path $root 'services/control-plane/src/create-admin.js')
  if ($LASTEXITCODE -ne 0) { throw 'Administrator creation failed.' }
} finally {
  Remove-Item Env:SETUP_ADMIN_USERNAME -ErrorAction SilentlyContinue
  Remove-Item Env:SETUP_ADMIN_PASSWORD -ErrorAction SilentlyContinue
  if ($passwordPtr -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($passwordPtr) }
  if ($confirmPtr -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($confirmPtr) }
  $plainPassword = $null
  $confirmPassword = $null
  $password.Dispose()
  $confirm.Dispose()
}
