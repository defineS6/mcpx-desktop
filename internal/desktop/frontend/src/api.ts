// 与 internal/desktop/api_windows.go 的 JSON 契约一一对应。
// 后端挂在 Wails AssetServer 的 /api 路由上，这里直接用相对路径 fetch。

export interface ServiceState {
  status: 'running' | 'starting' | 'conflict' | 'stopped'
  pid: number
  addr: string
  endpoint: string
  auth_mode: string
  executable: string
  home_dir: string
  log_path: string
  config_path: string
  error?: string
}

// MCPX 内核程序状态信息（与 GET/PUT /api/runtime 契约对应）
export interface RuntimeInfo {
  path: string
  resolved_path: string
  available: boolean
  version: string
  error?: string
  // 桌面端内核路径设置文件（desktop-manager.json），非 MCPX 运行时的 config.yaml
  config_path: string
}

export interface WorkspaceItem {
  name: string
  path: string
  description: string
  missing: boolean
}

export interface ConnectionConfig {
  host: string
  port: number
  auth_mode: string
  token: string
  oauth_server_url: string
  effective_mode: string
}

export interface CloudflareConfig {
  mode: 'quick' | 'named'
  tunnel_token: string
  tunnel_id: string
  public_url: string
  last_public_url: string
  manage_with_mcpx: boolean
  sync_oauth_server_url: boolean
  auto_recover: boolean
}

export interface CloudflareSoftwareStatus {
  installed: boolean
  managed: boolean
  path: string
  version: string
}

export interface CloudflareState {
  status: 'running' | 'starting' | 'stopped' | 'error'
  pid: number
  mode: 'quick' | 'named'
  tunnel_id: string
  public_url: string
  public_mcp_url: string
  local_mcp_url: string
  oauth_server_url: string
  oauth_linked: boolean
  log_path: string
  config_path: string
  executable: string
  software: CloudflareSoftwareStatus
  error?: string
}

export interface CloudflareHealthItem {
  ok: boolean
  detail: string
  url?: string
  status_code?: number
}

export interface CloudflareHealth {
  ok: boolean
  checked_at: string
  software: CloudflareHealthItem
  local_mcp: CloudflareHealthItem
  tunnel_process: CloudflareHealthItem
  public_mcp: CloudflareHealthItem
  oauth_metadata: CloudflareHealthItem
  oauth_linked: boolean
  diagnosis?: string
}

export interface LogChunk {
  content: string
  offset: number
  next_offset: number
  size: number
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`/api${path}`, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const text = await response.text()
  const payload = text ? JSON.parse(text) : null
  if (!response.ok) {
    throw new Error(payload?.error ?? `请求失败（${response.status}）`)
  }
  return payload as T
}

export const api = {
  status: () => request<ServiceState>('/status'),

  // 获取内核程序状态信息
  getRuntime: () => request<RuntimeInfo>('/runtime'),

  // 更新内核程序路径设置（传入空字符串代表恢复自动发现；运行时不允许改路径，后端会报错）
  putRuntime: (path: string) =>
    request<RuntimeInfo>('/runtime', {
      method: 'PUT',
      body: JSON.stringify({ path }),
    }),

  // 调起系统原生文件选择对话框选择内核程序文件，取消则返回空路径
  pickRuntime: () =>
    request<{ path: string }>('/runtime/pick', { method: 'POST' }),

  serviceAction: (action: 'start' | 'stop' | 'restart') =>
    request<ServiceState>(`/service/${action}`, { method: 'POST' }),

  listWorkspaces: () => request<WorkspaceItem[]>('/workspaces'),

  addWorkspace: (path: string, description: string) =>
    request<WorkspaceItem[]>('/workspaces', {
      method: 'POST',
      body: JSON.stringify({ path, description }),
    }),

  patchWorkspace: (name: string, description: string) =>
    request<WorkspaceItem[]>(`/workspaces/${encodeURIComponent(name)}`, {
      method: 'PATCH',
      body: JSON.stringify({ description }),
    }),

  deleteWorkspace: (name: string) =>
    request<WorkspaceItem[]>(`/workspaces/${encodeURIComponent(name)}`, {
      method: 'DELETE',
    }),

  pickDirectory: () =>
    request<{ path: string }>('/pick-directory', { method: 'POST' }),

  readLogs: (offset: number) => request<LogChunk>(`/logs?offset=${offset}`),

  clearLogs: () => request<{ ok: boolean }>('/logs/clear', { method: 'POST' }),

  getConfig: () => request<ConnectionConfig>('/config'),

  putConfig: (config: ConnectionConfig) =>
    request<ConnectionConfig>('/config', {
      method: 'PUT',
      body: JSON.stringify(config),
    }),

  generateToken: () =>
    request<{ token: string }>('/config/token', { method: 'POST' }),

  cloudflareStatus: () => request<CloudflareState>('/cloudflare/status'),

  getCloudflareConfig: () => request<CloudflareConfig>('/cloudflare/config'),

  putCloudflareConfig: (config: CloudflareConfig) =>
    request<CloudflareConfig>('/cloudflare/config', {
      method: 'PUT',
      body: JSON.stringify(config),
    }),

  installCloudflared: () =>
    request<CloudflareSoftwareStatus>('/cloudflare/install', { method: 'POST' }),

  uninstallCloudflared: () =>
    request<CloudflareSoftwareStatus>('/cloudflare/uninstall', { method: 'POST' }),

  startCloudflare: () =>
    request<CloudflareState>('/cloudflare/start', { method: 'POST' }),

  stopCloudflare: () =>
    request<CloudflareState>('/cloudflare/stop', { method: 'POST' }),

  cloudflareHealth: () =>
    request<CloudflareHealth>('/cloudflare/health', { method: 'POST' }),

  readCloudflareLogs: (offset: number) =>
    request<LogChunk>(`/cloudflare/logs?offset=${offset}`),

  clearCloudflareLogs: () =>
    request<{ ok: boolean }>('/cloudflare/logs/clear', { method: 'POST' }),

  open: (target: 'home' | 'log' | 'config' | 'cloudflare-log' | 'cloudflare-config') =>
    request<{ ok: boolean }>('/open', {
      method: 'POST',
      body: JSON.stringify({ target }),
    }),
}
