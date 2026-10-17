param(
    [ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$')]
    [string]$Version = '0.1.0'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$frontend = Join-Path $root 'internal\desktop\frontend'

# 1. 编译前端资源
Push-Location $frontend
try {
    npm ci
    if ($LASTEXITCODE -ne 0) { throw "Frontend dependencies installation failed" }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "Frontend build failed" }
} finally { Pop-Location }

# 2. 自动化测试、静态检查与无控制台独立二进制编译
Push-Location $root
$oldCGO = $env:CGO_ENABLED
try {
    go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { throw "Go tests failed" }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "Go vet failed" }
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags "-H windowsgui -s -w -X main.version=$Version" -o bin/mcpx-desktop.exe ./cmd/mcpx-desktop
    if ($LASTEXITCODE -ne 0) { throw "Go build failed" }
    Write-Output "Build completed: $(Join-Path $root 'bin\mcpx-desktop.exe')"
} finally {
    $env:CGO_ENABLED = $oldCGO
    Pop-Location
}
