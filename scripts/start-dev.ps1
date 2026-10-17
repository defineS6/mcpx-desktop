$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$frontend = Join-Path $root 'internal\desktop\frontend'
$exe = Join-Path $root 'bin\mcpx-desktop.exe'

$running = Get-Process -Name mcpx-desktop -ErrorAction SilentlyContinue
if ($running) {
    Write-Output "MCPX Desktop is already running (PID: $($running.Id))"
    exit 0
}

$node = (Get-Command node.exe).Source
$vite = Start-Process -FilePath $node -ArgumentList 'node_modules/vite/bin/vite.js --host 127.0.0.1 --port 5173 --strictPort' -WorkingDirectory $frontend -WindowStyle Hidden -PassThru

$ready = $false
for ($i = 0; $i -lt 15; $i++) {
    Start-Sleep -Milliseconds 400
    try {
        $res = Invoke-WebRequest -Uri 'http://127.0.0.1:5173' -UseBasicParsing -TimeoutSec 1
        if ($res.StatusCode -eq 200) { $ready = $true; break }
    } catch {}
}

if (-not $ready) {
    if ($vite -and -not $vite.HasExited) { Stop-Process -Id $vite.Id -Force }
    throw 'Vite dev server failed to start'
}

$runtime = 'D:\Apps\Mcpx\mcpx.exe'
$desktop = Start-Process -FilePath $exe -ArgumentList "-dev-url http://127.0.0.1:5173 -runtime `"$runtime`"" -WorkingDirectory $root -PassThru
Write-Output "MCPX Desktop started (PID: $($desktop.Id), Vite PID: $($vite.Id))"
