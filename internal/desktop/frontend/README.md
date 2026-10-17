# MCPX Desktop 前端工程

MCPX Desktop 桌面端管理器前端，基于 Vue 3 + TypeScript + Vite 构建，挂载在 Wails AssetServer 上运行。

## 架构原则与定位

1. **桌面管理器与官方内核分离**：
   - MCPX Desktop 负责托盘、UI、公网 Tunnel 与进程看护；
   - 官方 `mcpx.exe` 作为独立可执行内核运行，前端通过 `/api/runtime` 检测可用性、版本与生效路径，并支持自动发现与手动路径配置；
   - 桌面配置记录于 `desktop-manager.json`（对应 `runtime.config_path`），与内核配置 `config.yaml`（对应 `state.config_path`）职责分离。
2. **状态与轮询机制**：
   - 定时轮询采用单重调用守卫（防并发重叠）；
   - 网络或服务错误在成功恢复后会自动清除残留提示；
   - 用户手工编辑的内核路径具有脏状态（Dirty State）保护机制，未保存状态下即便控件失焦也不会被后台轮询覆盖；
   - 本地服务或 Cloudflare Tunnel 运行期间，修改路径相关操作按钮与输入框处于锁定禁用状态。
3. **敏感信息保护**：
   - 连接配置中的 Bearer Token 默认采用 `type="password"` 与 `autocomplete="off"`，并提供显隐切换控件；
   - Cloudflare Tunnel Token 采用密码框输入；保存在运行目录，不进入进程参数或源码仓库。
4. **配置安全与注释保留**：
   - 后端写入 `config.yaml` 时基于 YAML 节点增量修改并自动生成 `.bak` 备份，保留既有注释和未知扩展字段；前端只提交受管字段。

## 目录结构

```
internal/desktop/frontend/
├── dist/                  # 打包构建产物（供 Wails 嵌入）
├── src/
│   ├── views/
│   │   ├── ServiceView.vue     # 主服务、内核管理与连接配置
│   │   ├── CloudflareView.vue  # Cloudflare Tunnel 托管与公网穿透
│   │   ├── WorkspaceView.vue   # 项目 Workspace 注册与管理
│   │   └── LogsView.vue        # 服务日志增量查看与清空
│   ├── api.ts                  # REST API 契约定义与请求包装
│   ├── App.vue                 # 顶级导航与视图切换
│   ├── main.ts                 # Vue 应用启动入口
│   ├── style.css               # 原生系统主题样式定义（无外部字体依赖）
│   └── theme.ts                # 深浅色主题切换逻辑
├── package.json
├── tsconfig.json
└── vite.config.ts
```

## API 契约列表

| 接口路径 | 方法 | 说明 |
| :--- | :--- | :--- |
| `/api/status` | GET | 获取本地 MCPX 守护进程及运行时状态 |
| `/api/runtime` | GET | 获取内核可用性、版本、生效路径及桌面配置文件路径 |
| `/api/runtime` | PUT | 保存内核路径（空字符串表示恢复自动发现模式） |
| `/api/runtime/pick` | POST | 唤起系统文件选择框选取 `mcpx.exe`，取消返回空路径 |
| `/api/service/{action}` | POST | 控制服务启停（`start` / `stop` / `restart`） |
| `/api/config` | GET | 读取连接配置（监听地址、端口、Token、OAuth Origin 等） |
| `/api/config` | PUT | 更新连接配置（保留未知字段和既有注释） |
| `/api/config/token` | POST | 随机生成高强度 32 字节 Bearer Token |
| `/api/cloudflare/status` | GET | 读取 Tunnel 运行状态及 cloudflared 软件版本 |
| `/api/cloudflare/config` | GET/PUT | 读取与保存 Tunnel 配置 |
| `/api/cloudflare/start` | POST | 手动启动 Cloudflare Tunnel |
| `/api/cloudflare/stop` | POST | 手动停止 Cloudflare Tunnel |
| `/api/cloudflare/health` | POST | 执行端到端健康检查（本地 MCP、Tunnel、OAuth 等） |
| `/api/workspaces` | GET/POST | 读取或注册项目 Workspace 目录 |
| `/api/workspaces/{name}` | PATCH/DELETE | 修改描述或移除 Workspace 注册 |
| `/api/logs` | GET | 增量读取 MCPX 服务日志 |
| `/api/logs/clear` | POST | 清空 MCPX 服务日志 |
| `/api/open` | POST | 在资源管理器或默认应用中打开目录或文件 |
