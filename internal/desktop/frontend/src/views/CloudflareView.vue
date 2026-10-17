<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import {
  api,
  type CloudflareConfig,
  type CloudflareHealth,
  type CloudflareHealthItem,
  type CloudflareState,
} from '../api'

const state = ref<CloudflareState | null>(null)
const config = ref<CloudflareConfig | null>(null)
const health = ref<CloudflareHealth | null>(null)
const error = ref('')
const hint = ref('')
const busy = ref(false)
const installBusy = ref(false)
const copied = ref(false)

const rawLogs = ref('')
const followLogs = ref(true)
const logViewport = ref<HTMLElement | null>(null)
let logOffset = 0
let statusTimer: number | undefined
let logTimer: number | undefined
let isRefreshingStatus = false
let isPollingLogs = false

const statusText: Record<string, string> = {
  running: 'Tunnel 运行中',
  starting: 'Tunnel 启动中',
  stopped: 'Tunnel 已停止',
  error: 'Tunnel 状态异常',
}

const softwareText = computed(() => {
  if (!state.value?.software.installed) return '未安装'
  const source = state.value.software.managed ? 'MCPX 管理' : '系统安装'
  return `${source}${state.value.software.version ? ` · ${state.value.software.version}` : ''}`
})

const healthItems = computed<{ label: string; value: CloudflareHealthItem }[]>(() => {
  if (!health.value) return []
  return [
    { label: 'cloudflared', value: health.value.software },
    { label: '本地 MCP', value: health.value.local_mcp },
    { label: 'Tunnel 进程', value: health.value.tunnel_process },
    { label: '公网 MCP', value: health.value.public_mcp },
    { label: 'OAuth metadata', value: health.value.oauth_metadata },
  ]
})

// 刷新 Cloudflare 状态与配置（防并发重叠，成功后清空旧错误）
async function refreshStatus() {
  if (isRefreshingStatus) return
  isRefreshingStatus = true
  try {
    state.value = await api.cloudflareStatus()
    if (!config.value) config.value = await api.getCloudflareConfig()
    // 请求成功后清空旧错误
    error.value = ''
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    isRefreshingStatus = false
  }
}

async function saveConfig(showHint = true) {
  if (!config.value) return
  try {
    config.value = await api.putCloudflareConfig(config.value)
    if (showHint) {
      hint.value = 'Cloudflare 配置已保存。'
      window.setTimeout(() => (hint.value = ''), 3000)
    }
  } catch (err) {
    throw err
  }
}

async function install() {
  installBusy.value = true
  error.value = ''
  hint.value = '正在从 Cloudflare 官方 GitHub Release 安装 cloudflared…'
  try {
    await api.installCloudflared()
    hint.value = 'cloudflared 已安装到 MCPX 运行时目录。'
    await refreshStatus()
  } catch (err) {
    error.value = (err as Error).message
    hint.value = ''
  } finally {
    installBusy.value = false
  }
}

async function uninstall() {
  installBusy.value = true
  error.value = ''
  try {
    await api.uninstallCloudflared()
    hint.value = '已移除 MCPX 管理的 cloudflared。系统安装不会被删除。'
    await refreshStatus()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    installBusy.value = false
  }
}

async function start() {
  busy.value = true
  error.value = ''
  try {
    await saveConfig(false)
    state.value = await api.startCloudflare()
    hint.value = 'Cloudflare Tunnel 已手动启动。'
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function stop() {
  busy.value = true
  error.value = ''
  try {
    state.value = await api.stopCloudflare()
    hint.value = 'Cloudflare Tunnel 已停止。'
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function runHealth() {
  error.value = ''
  try {
    health.value = await api.cloudflareHealth()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function copyPublicURL() {
  if (!state.value?.public_mcp_url) return
  await navigator.clipboard.writeText(state.value.public_mcp_url)
  copied.value = true
  window.setTimeout(() => (copied.value = false), 1500)
}

// 轮询增量读取 Cloudflare 日志（防并发重叠）
async function pollLogs() {
  if (isPollingLogs) return
  isPollingLogs = true
  try {
    const chunk = await api.readCloudflareLogs(logOffset)
    if (chunk.offset === 0 && logOffset !== 0) rawLogs.value = ''
    if (chunk.content) {
      rawLogs.value += chunk.content
      if (followLogs.value) {
        await nextTick()
        const element = logViewport.value
        if (element) element.scrollTop = element.scrollHeight
      }
    }
    logOffset = chunk.next_offset
  } catch (err) {
    // 轮询日志若出错不覆盖主状态错误
  } finally {
    isPollingLogs = false
  }
}

async function clearLogs() {
  try {
    await api.clearCloudflareLogs()
    rawLogs.value = ''
    logOffset = 0
  } catch (err) {
    error.value = (err as Error).message
  }
}

onMounted(async () => {
  await refreshStatus()
  await pollLogs()
  statusTimer = window.setInterval(refreshStatus, 2000)
  logTimer = window.setInterval(pollLogs, 1000)
})

onUnmounted(() => {
  window.clearInterval(statusTimer)
  window.clearInterval(logTimer)
})
</script>

<template>
  <div class="page">
    <div v-if="error" class="banner">{{ error }}</div>
    <div v-if="state?.error" class="banner">{{ state.error }}</div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">cloudflared</h2>
        <span class="spacer"></span>
        <strong>{{ softwareText }}</strong>
      </div>
      <p class="hint mono-path">{{ state?.software.path || '尚未检测到 cloudflared' }}</p>
      <div class="row" style="margin-top: 12px">
        <button
          class="btn primary"
          :disabled="installBusy || (state?.status === 'running' && state?.software.managed)"
          @click="install"
        >
          {{ installBusy ? '处理中…' : state?.software.installed ? '重新安装 / 更新' : '安装 cloudflared' }}
        </button>
        <button
          class="btn danger"
          :disabled="installBusy || !state?.software.managed || state?.status === 'running'"
          @click="uninstall"
        >
          卸载受管版本
        </button>
        <button class="btn" @click="refreshStatus">重新检测</button>
      </div>
      <p class="hint">支持自动检测系统 PATH 中的 cloudflared，或自动安装到 ~/.mcpx/bin/。</p>
    </div>

    <div v-if="config" class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">Tunnel 配置</h2>
        <span class="spacer"></span>
        <button class="btn" :disabled="busy" @click="saveConfig()">保存配置</button>
      </div>

      <div class="row" style="margin-top: 14px; align-items: flex-end">
        <label class="field">
          模式
          <select v-model="config.mode">
            <option value="named">固定域名穿透（Named - 推荐 ChatGPT 网页端）</option>
            <option value="quick">临时调试穿透（Quick - 随机域名）</option>
          </select>
        </label>
        <label class="checkbox" style="margin-bottom: 7px">
          <input v-model="config.manage_with_mcpx" type="checkbox" />
          随 MCPX 启停 Cloudflare Tunnel
        </label>
        <label class="checkbox" style="margin-bottom: 7px">
          <input v-model="config.sync_oauth_server_url" type="checkbox" />
          公网 Origin 自动联动 OAuth server_url
        </label>
        <label class="checkbox" style="margin-bottom: 7px">
          <input v-model="config.auto_recover" type="checkbox" />
          自动健康检查并故障恢复
        </label>
      </div>

      <template v-if="config.mode === 'named'">
        <div class="row" style="margin-top: 12px; align-items: flex-end">
          <label class="field" style="flex: 1">
            隧道 Token (Tunnel Token)
            <input v-model="config.tunnel_token" type="password" autocomplete="off" placeholder="从 Cloudflare Zero Trust 复制的 eyJ… 密钥" />
          </label>
          <label class="field" style="width: 280px">
            Tunnel ID（可选，用于识别）
            <input v-model="config.tunnel_id" type="text" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" />
          </label>
        </div>
        <label class="field" style="margin-top: 12px">
          你的固定公网域名 (Origin)
          <input v-model="config.public_url" type="text" placeholder="https://mcpx.justnew.net" />
        </label>
        <p class="hint">
          需在 Cloudflare 零信任控制台配置域名路由，Token 仅保存在本地。
        </p>
      </template>
      <p v-else class="hint">
        Quick 模式随机分配临时域名，仅供临时调试，不支持 SSE。
      </p>
      <p class="hint">
        开启自动恢复后将定时进行健康检查并自动拉起，手动停止则暂停自动恢复。
      </p>
    </div>

    <div v-if="state" class="card">
      <div class="row">
        <span class="status-dot" :class="state.status === 'error' ? 'conflict' : state.status"></span>
        <span class="status-headline">{{ statusText[state.status] }}</span>
        <span class="spacer"></span>
        <button
          class="btn primary"
          :disabled="busy || state.status === 'running' || state.status === 'starting'"
          @click="start"
        >
          启动公网 Tunnel
        </button>
        <button class="btn" :disabled="busy || state.status !== 'running'" @click="stop">停止公网 Tunnel</button>
        <button class="btn" :disabled="busy" @click="runHealth">健康检查</button>
      </div>

      <p class="hint">可在上方勾选是否与本地 MCPX 服务保持联动启停。</p>

      <dl class="facts" style="margin-top: 14px">
        <dt>本地 MCP</dt>
        <dd>{{ state.local_mcp_url || '—' }}</dd>
        <dt>公网 MCP</dt>
        <dd>{{ state.public_mcp_url || '—' }}</dd>
        <dt>OAuth server_url</dt>
        <dd>{{ state.oauth_server_url || '—' }}</dd>
        <dt>OAuth 联动</dt>
        <dd>{{ state.public_url ? (state.oauth_linked ? '一致' : '未同步') : '—' }}</dd>
        <dt>Tunnel</dt>
        <dd>{{ state.pid > 0 ? `pid=${state.pid}` : '—' }}{{ state.tunnel_id ? ` · ${state.tunnel_id}` : '' }}</dd>
      </dl>

      <div class="row" style="margin-top: 12px">
        <button class="btn" :disabled="!state.public_mcp_url" @click="copyPublicURL">
          {{ copied ? '已复制' : '复制公网 MCP URL' }}
        </button>
        <button class="btn" @click="api.open('cloudflare-log')">打开 Cloudflare 日志</button>
        <button class="btn" @click="api.open('cloudflare-config')">打开 Tunnel 配置</button>
      </div>
      <p v-if="hint" class="hint" style="color: var(--ok)">{{ hint }}</p>
    </div>

    <div v-if="health" class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">健康检查</h2>
        <span class="spacer"></span>
        <strong :style="{ color: health.ok ? 'var(--ok)' : 'var(--warn)' }">
          {{ health.ok ? '全部通过' : '存在异常' }}
        </strong>
      </div>
      <p v-if="health.diagnosis" class="hint" style="margin-top: 10px">
        诊断：{{ health.diagnosis }}
      </p>
      <div class="health-grid">
        <div v-for="item in healthItems" :key="item.label" class="health-item">
          <div class="row">
            <span class="status-dot" :class="item.value.ok ? 'running' : 'conflict'"></span>
            <strong>{{ item.label }}</strong>
          </div>
          <div class="hint">{{ item.value.detail || '未检查' }}</div>
          <div v-if="item.value.url" class="hint mono-path">{{ item.value.url }}</div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">Cloudflare 日志</h2>
        <label class="checkbox">
          <input v-model="followLogs" type="checkbox" />
          自动滚动
        </label>
        <span class="spacer"></span>
        <button class="btn danger" @click="clearLogs">清空</button>
      </div>
      <pre ref="logViewport" class="log-view cloudflare-log">{{ rawLogs || '暂无 cloudflared 日志。' }}</pre>
    </div>
  </div>
</template>

<style scoped>
.mono-path {
  overflow-wrap: anywhere;
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}

.health-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 10px;
  margin-top: 14px;
}

.health-item {
  padding: 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--bg-input);
}

.cloudflare-log {
  height: 220px;
  margin-top: 12px;
}
</style>
