# MCPX Desktop

独立维护的 Windows MCPX 桌面管理器，基于 `opentokenz/mcpx v0.9.18` 的桌面模块拆分。管理器不包含 MCP 服务端、MCP SDK 或数据库实现；官方 `mcpx.exe` 与桌面端分别更新。

```text
mcpx-desktop.exe
├─ 外部 mcpx.exe：MCP Runtime、鉴权、项目工具
└─ 外部 cloudflared.exe：Cloudflare Tunnel
```

## 功能

- 原有 Vue 界面、系统托盘、浅色/深色/系统主题。
- 外部内核路径选择、版本检测；无黑窗启动、停止和重启。
- Quick / Named Tunnel、固定公网 Origin、OAuth 地址联动、独立日志。
- 可选服务与 Tunnel 联动，以及连续失败后的分层恢复和冷却。
- 工作区注册、修改描述和移除注册；移除注册不删除项目文件。
- 配置按 YAML 节点增量修改，保留未知字段和注释，自动备份并检测加载后发生的文件变化。

## 使用

需要 Windows 10/11、WebView2 Runtime，以及单独下载的官方 Windows MCPX 内核。

1. 将 `mcpx-desktop.exe` 放在自己选定的目录。
2. 将官方 `mcpx.exe` 放在同目录，或在“服务 → 内核程序”选择其他位置。
3. 双击管理器。启动管理器不会自动启动 Runtime 或 Tunnel。
4. 按需要配置鉴权、端口和工作区，然后点击启动。

内核发现顺序为：已保存路径 → `MCPX_RUNTIME` → 管理器同目录 `mcpx.exe` → 同目录 `runtime/mcpx.exe` → PATH。设置了路径却失效时明确报错，不悄悄启动另一个内核。

服务或 Tunnel 运行时禁止修改内核路径。版本检测只调用 `-version`，不会自动升级、替换官方内核，也不承诺兼容任意未来版本。

### 固定域名

选择 **Named**，填写该命名隧道的 Tunnel Token 与 `https://mcp.example.com` 形式的公网 Origin。域名路由仍需先在 Cloudflare 控制台配置到本机 MCPX 服务地址；这里不会替你创建 DNS 或迁移隧道。

- Origin 不追加 `/mcp`；对外 MCP 地址会自动追加。
- Token 不写入进程参数，通过 `TUNNEL_TOKEN` 环境变量传递。
- 拒绝将 `auth.mode=open` 的服务暴露到公网。
- 如果另有 Windows Cloudflared 服务使用同一个隧道，应由用户明确决定使用哪一个管理入口；本管理器不会擅自停用系统服务。
- 自动安装仅操作本工具自管的 cloudflared，并核验官方 Release 的 SHA-256。元数据不可用或摘要缺失时保留原程序并报错。

### 退出和开机自启

- 关闭窗口：隐藏到托盘。
- 退出托盘：退出管理器；已启动的 Runtime 和 Tunnel 继续运行，但不再执行桌面健康检查及自动恢复。
- 停止服务：遵循“随 MCPX 启停 Tunnel”选项；手动停止 Tunnel 会撤销自动恢复意图。
- 开机自启：只启动管理器并驻留托盘，不自动启动服务。

不要让旧版桌面端与本管理器同时控制同一个运行目录，尤其不要同时启用两个 watchdog。

## 文件与凭据

默认沿用 `~/.mcpx`，可通过 `MCPX_HOME` 或管理器 `-home` 参数使用其他目录。

| 文件 | 所有者及用途 |
|---|---|
| `desktop-manager.json` | 桌面专属内核路径设置 |
| `config.yaml` | 官方 Runtime 配置；桌面只修改明确管理的字段 |
| `config.yaml.bak` | 桌面保存前的原文件备份 |
| `mcpx-daemon.json` | 官方 Runtime PID 与镜像路径 |
| `cloudflare-desktop.json` | Tunnel 配置，包含敏感 Token |
| `cloudflare-desktop-state.json` | 本管理器跟踪的 Tunnel 进程 |
| `logs/` | 服务日志与 Tunnel 日志 |

所有新 JSON 均写为 UTF-8 无 BOM，也接受外部编辑器加入的 UTF-8 BOM。配置无损是保留字段和注释，不保证字节级排版完全不变。

快照冲突检查能拒绝加载后已发生的外部修改，但不能把不配合文件锁的第三方进程纳入原子事务。Windows 的文件权限模式不等于独立设置了 NTFS ACL；请保护运行目录，不要把配置、凭据、日志或数据库提交到仓库。

## 开发

需要 Go 1.26.1+、Node.js 22+、npm。Go 可以按 `go.mod` 自动下载匹配工具链。

```powershell
# 自动化测试与静态检查，不启动真实 Tunnel
 go test ./... -count=1
 go vet ./...

# 完整发布构建：先生成前端，再测试并编译无控制台 EXE
 .\scripts\build.ps1
```

发布版必须先生成前端资源协议清单。若资源缺失或仍为上游旧版，管理器会明确拒绝启动，不伪装成最新界面。

### 不打包的开发预览

在前端目录运行：

```powershell
npm ci
npm run dev -- --host 127.0.0.1 --port 5173 --strictPort
```

再在项目根目录编译管理器并显式选择开发地址：

```powershell
go build -ldflags '-H windowsgui' -o bin/mcpx-desktop.exe ./cmd/mcpx-desktop
.\bin\mcpx-desktop.exe -dev-url http://127.0.0.1:5173 -home C:\Temp\mcpx-desktop-dev
```

开发代理只接受回环 IP，不允许远程网站。开发预览依赖本机 Vite 进程，不生成发布资源，不能作为免打包的发行版。

### 真实内核隔离测试

```powershell
$env:MCPX_TEST_RUNTIME = 'C:\Tools\mcpx\mcpx.exe'
go test ./internal/desktop -run '^TestExternalRuntimeLifecycle$' -count=1 -v
Remove-Item Env:\MCPX_TEST_RUNTIME
```

该测试使用临时 `MCPX_HOME`、空闲端口和测试凭据，执行真实启动、MCP initialize、重启及停止；默认测试不会自动选择或操作现有服务。

仓库的 GitHub Actions 在 Windows 上执行前端构建、Go 测试、竞态检测和打包，并上传可下载归档。工作流不自动发布 Release 或写回仓库。

## 来源与许可

上游：[opentokenz/mcpx](https://github.com/opentokenz/mcpx)，基线 `v0.9.18 / d17e68a`。
独立仓库：[defineS6/mcpx-desktop](https://github.com/defineS6/mcpx-desktop)。

沿用 Apache-2.0；保留 `LICENSE`、`NOTICE` 与既有来源说明。
