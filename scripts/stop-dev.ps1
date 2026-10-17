$ErrorActionPreference = 'SilentlyContinue'

Get-Process -Name mcpx-desktop | Stop-Process -Force

$conn = Get-NetTCPConnection -LocalPort 5173 -State Listen
if ($conn) {
    Stop-Process -Id $conn.OwningProcess -Force
}

Write-Output 'MCPX Desktop test environment stopped.'
