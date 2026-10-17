param(
    [ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$')]
    [string]$Version = '0.1.0'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$frontend = Join-Path $root 'internal\desktop\frontend'

# 完整发布构建必须先生成前端，不能跳过并使用旧版资源。
Push-Location $frontend
try {
    npm ci
    if ($LASTEXITCODE -ne 0) { throw '前端依赖安装失败' }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw '前端类型检查或资源打包失败' }
} finally { Pop-Location }

Push-Location $root
$oldCGO = $env:CGO_ENABLED
try {
    go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { throw '自动化测试失败' }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw '静态检查失败' }
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags "-H windowsgui -s -w -X main.version=$Version" -o bin/mcpx-desktop.exe ./cmd/mcpx-desktop
    if ($LASTEXITCODE -ne 0) { throw '桌面程序编译失败' }
    Write-Output "构建完成：$(Join-Path $root 'bin\mcpx-desktop.exe')"
} finally {
    $env:CGO_ENABLED = $oldCGO
    Pop-Location
}
