<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import {
  api,
  type CloudflareConfig,
  type CloudflareState,
  type ConnectionConfig,
  type RuntimeInfo,
  type ServiceState,
} from '../api'

const state = ref<ServiceState | null>(null)
const cloudflare = ref<CloudflareState | null>(null)
const runtime = ref<RuntimeInfo | null>(null)
const runtimePathInput = ref('')
const runtimeBusy = ref(false)
const runtimeHint = ref('')
const runtimeError = ref('')
const isEditingPath = ref(false)
let hasInitializedInput = false
let isRefreshing = false

const cloudflareConfig = ref<CloudflareConfig | null>(null)
const config = ref<ConnectionConfig | null>(null)
const error = ref('')
const busy = ref(false)
const copied = ref(false)
const showConnection = ref(false)
const showRuntime = ref(false)
const showToken = ref(false)
const savedHint = ref('')

let timer: number | undefined

const statusText: Record<string, string> = {
  running: '运行中',
  starting: '启动中（端口尚未就绪）',
  conflict: '端口被其他进程占用',
  stopped: '已停止',
}

// 过滤掉冗余的 commit 和 build date 等括号元数据，仅保留核心版本号
function formatVersion(raw?: string): string {
  if (!raw) return ''
  return raw.replace(/\s*\([^)]*\)/g, '').trim()
}

// 本地 MCPX 服务是否正处于启动或运行态
const isServiceRunning = computed(
  () => state.value?.status === 'running' || state.value?.status === 'starting',
)

// Cloudflare Tunnel 是否正处于启动或运行态
const isTunnelRunning = computed(
  () => cloudflare.value?.status === 'running' || cloudflare.value?.status === 'starting',
)

// 服务或 Tunnel 运行时禁止修改内核路径（保持运行环境稳定，后端亦做拦截）
const isRuntimeLocked = computed(
  () => isServiceRunning.value || isTunnelRunning.value,
)

// 用户是否手动修改了路径输入框（输入内容与当前后端保存路径不一致，处于脏状态）
const isRuntimePathDirty = computed(() => {
  const savedPath = runtime.value?.path || ''
  return runtimePathInput.value !== savedPath
})

// 受托盘/CLI 管理的进程才可以停止或重启。端口冲突时的占用者不归我们管，
// 也不该被我们停掉。
const managed = computed(() => {
  const localManaged = state.value?.status === 'running' || state.value?.status === 'starting'
  if (cloudflareConfig.value?.manage_with_mcpx === false) return localManaged
  return localManaged || cloudflare.value?.status === 'running' || cloudflare.value?.status === 'starting'
})

const startSatisfied = computed(() => {
  if (state.value?.status !== 'running') return false
  if (cloudflareConfig.value?.manage_with_mcpx === false) return true
  return cloudflare.value?.status === 'running'
})

// 刷新主服务、Cloudflare 状态与内核程序状态（防 interval 重叠，并在成功后清空旧错误）
async function refresh() {
  if (isRefreshing) return
  isRefreshing = true
  try {
    const [serviceState, cloudflareState, tunnelConfig, runtimeInfo, connConfig] = await Promise.all([
      api.status(),
      api.cloudflareStatus(),
      api.getCloudflareConfig(),
      api.getRuntime(),
      api.getConfig().catch(() => null),
    ])
    state.value = serviceState
    cloudflare.value = cloudflareState
    cloudflareConfig.value = tunnelConfig
    runtime.value = runtimeInfo
    if (connConfig) config.value = connConfig

    // 请求成功后清空旧的接口通信错误
    error.value = ''

    // 脏状态保护：首次初始化，或输入框无焦点且未被手动编辑（非脏状态）时，才同步最新路径
    if (!hasInitializedInput) {
      runtimePathInput.value = runtimeInfo.path || ''
      hasInitializedInput = true
    } else if (!isEditingPath.value && !isRuntimePathDirty.value) {
      runtimePathInput.value = runtimeInfo.path || ''
    }
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    isRefreshing = false
  }
}

// 放弃未保存的手工输入，恢复为当前后端生效的配置
function revertRuntimePath() {
  runtimePathInput.value = runtime.value?.path || ''
}

// 调起原生文件选择框选取内核可执行文件
async function pickRuntimeFile() {
  runtimeBusy.value = true
  runtimeError.value = ''
  try {
    const result = await api.pickRuntime()
    if (result && result.path) {
      runtimePathInput.value = result.path
    }
  } catch (err) {
    runtimeError.value = (err as Error).message
  } finally {
    runtimeBusy.value = false
  }
}

// 保存内核程序路径配置（留空即恢复自动发现）
async function saveRuntimePath() {
  runtimeBusy.value = true
  runtimeError.value = ''
  runtimeHint.value = ''
  try {
    const updated = await api.putRuntime(runtimePathInput.value.trim())
    runtime.value = updated
    runtimePathInput.value = updated.path || ''
    runtimeHint.value = '内核路径已保存并更新检测结果。'
    window.setTimeout(() => (runtimeHint.value = ''), 4000)
    await refresh()
  } catch (err) {
    runtimeError.value = (err as Error).message
  } finally {
    runtimeBusy.value = false
  }
}

// 重置路径配置并恢复自动发现
async function resetToAutoDiscover() {
  runtimePathInput.value = ''
  await saveRuntimePath()
}

// 手动重新检测内核程序
async function recheckRuntime() {
  runtimeBusy.value = true
  runtimeError.value = ''
  try {
    const info = await api.getRuntime()
    runtime.value = info
    runtimePathInput.value = info.path || ''
    runtimeHint.value = '内核状态已重新检测。'
    window.setTimeout(() => (runtimeHint.value = ''), 3000)
  } catch (err) {
    runtimeError.value = (err as Error).message
  } finally {
    runtimeBusy.value = false
  }
}

async function act(action: 'start' | 'stop' | 'restart') {
  busy.value = true
  error.value = ''
  try {
    state.value = await api.serviceAction(action)
    cloudflare.value = await api.cloudflareStatus()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function copyEndpoint() {
  if (!state.value) return
  await navigator.clipboard.writeText(state.value.endpoint)
  copied.value = true
  window.setTimeout(() => (copied.value = false), 1500)
}

async function loadConfig() {
  try {
    config.value = await api.getConfig()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function toggleConnection() {
  showConnection.value = !showConnection.value
  if (showConnection.value && !config.value) {
    await loadConfig()
  }
}

async function generateToken() {
  if (!config.value) return
  try {
    config.value.token = (await api.generateToken()).token
  } catch (err) {
    error.value = (err as Error).message
  }
}

// 保存后不自动重启：正在跑的 MCPX 使用的还是旧配置，由用户决定何时重启整套服务。
async function saveConfig() {
  if (!config.value) return
  busy.value = true
  error.value = ''
  try {
    config.value = await api.putConfig(config.value)
    savedHint.value = '已保存。改动在下次重启 MCPX 后生效。'
    window.setTimeout(() => (savedHint.value = ''), 4000)
    await refresh()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 2000)
})

onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <div class="page">
    <div v-if="error" class="banner">{{ error }}</div>
    <div v-if="state?.error" class="banner">{{ state.error }}</div>

    <div v-if="state" class="card">
        <div class="row">
          <span class="status-dot" :class="state.status"></span>
          <span class="status-headline">{{ statusText[state.status] }}</span>
          <span class="spacer"></span>
          <button
            class="btn primary"
            :disabled="busy || startSatisfied || state.status === 'conflict' || state.status === 'starting' || !runtime?.available"
            :title="!runtime?.available ? '未检测到可用内核，无法启动' : undefined"
            @click="act('start')"
          >
            启动 MCPX
          </button>
          <button class="btn" :disabled="busy || !managed" @click="act('stop')">
            停止 MCPX
          </button>
          <button
            class="btn"
            :disabled="busy || !managed || !runtime?.available"
            :title="!runtime?.available ? '未检测到可用内核，无法重启' : undefined"
            @click="act('restart')"
          >
            重启 MCPX
          </button>
        </div>

        <p v-if="cloudflareConfig?.manage_with_mcpx !== false" class="hint">
          已启用服务联动：“启动 / 停止 / 重启”将同时作用于本地 Runtime 与 Cloudflare Tunnel。
        </p>
        <p v-else class="hint">
          未启用服务联动：此处仅控制本地 Runtime，Cloudflare Tunnel 需在对应页面单独管理。
        </p>

        <p v-if="!runtime?.available && state.status !== 'running'" class="hint" style="color: var(--warn)">
          未检测到可用内核，服务已禁用。请在下方配置 mcpx.exe 路径。
        </p>

        <p v-if="state.status === 'conflict'" class="hint" style="color: var(--warn)">
          端口 {{ state.addr }} 已被非受管进程占用，请先结束该进程或更换端口。
        </p>

        <dl class="facts">
          <dt>MCP 端点</dt>
          <dd>{{ state.endpoint }}</dd>
          <dt>鉴权模式</dt>
          <dd>{{ state.auth_mode }}</dd>
          <dt>进程</dt>
          <dd>{{ state.pid > 0 ? `pid=${state.pid}` : '—' }}</dd>
          <dt>Cloudflare Tunnel</dt>
          <dd>{{ cloudflare?.status === 'running' ? `运行中 · pid=${cloudflare.pid}` : cloudflare?.status || '—' }}</dd>
          <dt>公网 MCP</dt>
          <dd>{{ cloudflare?.public_mcp_url || '—' }}</dd>
          <dt>运行时目录</dt>
          <dd>{{ state.home_dir }}</dd>
        </dl>

        <div class="row" style="margin-top: 14px">
          <button class="btn" @click="copyEndpoint">
            {{ copied ? '已复制' : '复制端点地址' }}
          </button>
          <button class="btn" @click="api.open('home')">打开运行时目录</button>
          <button class="btn" @click="api.open('config')">打开 config.yaml</button>
        </div>
      </div>

      <div v-else class="empty">读取状态中…</div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">内核程序</h2>
        <span
          class="status-dot"
          :class="{
            running: runtime?.available,
            stopped: !runtime?.available,
          }"
        ></span>
        <span class="status-headline" style="font-size: 14px">
          {{
            runtime?.available
              ? (runtime.version ? `就绪 · ${formatVersion(runtime.version)}` : '内核可用')
              : (runtime?.error ? '内核不可用' : '未检测到内核')
          }}
        </span>
        <span class="spacer"></span>
        <button class="btn" :disabled="runtimeBusy" @click="recheckRuntime">重新检测</button>
        <button class="btn" @click="showRuntime = !showRuntime">
          {{ showRuntime ? '收起' : '高级/更换路径' }}
        </button>
      </div>

      <template v-if="showRuntime || !runtime?.available">
        <p class="hint">
          管理器与官方内核独立更新，默认自动发现同目录 mcpx.exe 或 PATH，亦可手动指定路径。
        </p>

        <div v-if="runtimeError" class="banner" style="margin-top: 10px">{{ runtimeError }}</div>
        <div v-else-if="runtime?.error" class="banner" style="margin-top: 10px">内核错误：{{ runtime.error }}</div>
        <div v-else-if="runtime && !runtime.available" class="banner" style="margin-top: 10px">
          未检测到内核程序，请将 mcpx.exe 放入同目录或在下方手动选取。
        </div>

        <dl v-if="runtime" class="facts">
          <dt>内核可用性</dt>
          <dd>
            <span :style="{ color: runtime.available ? 'var(--ok)' : 'var(--danger)', fontWeight: 600 }">
              {{ runtime.available ? '可用 (Ready)' : '不可用 (Unavailable)' }}
            </span>
          </dd>
          <dt>内核版本</dt>
          <dd :title="runtime.version">{{ formatVersion(runtime.version) || (runtime.available ? '未知版本' : '未检测到（内核缺失）') }}</dd>
          <dt>生效路径</dt>
          <dd class="path">{{ runtime.resolved_path || '未找到可执行文件' }}</dd>
          <dt>发现模式</dt>
          <dd>{{ runtime.path ? '自定义固定路径' : '自动发现（同目录 mcpx.exe 或 PATH）' }}</dd>
        </dl>

        <div class="row" style="margin-top: 14px; align-items: flex-end">
          <label class="field" style="flex: 1">
            内核程序路径（留空代表自动发现）
            <input
              v-model="runtimePathInput"
              type="text"
              placeholder="留空自动发现管理器同目录 mcpx.exe 或 PATH"
              :disabled="runtimeBusy || isRuntimeLocked"
              @focus="isEditingPath = true"
              @blur="isEditingPath = false"
              @keydown.enter="saveRuntimePath"
            />
          </label>
          <button
            class="btn"
            :disabled="runtimeBusy || isRuntimeLocked"
            title="调起文件选择器选取 mcpx.exe"
            @click="pickRuntimeFile"
          >
            选择文件…
          </button>
          <button
            class="btn primary"
            :disabled="runtimeBusy || isRuntimeLocked"
            @click="saveRuntimePath"
          >
            保存
          </button>
          <button
            v-if="isRuntimePathDirty"
            class="btn"
            :disabled="runtimeBusy || isRuntimeLocked"
            title="放弃未保存的手工输入，恢复为当前已生效配置"
            @click="revertRuntimePath"
          >
            还原
          </button>
          <button
            v-if="runtimePathInput"
            class="btn"
            :disabled="runtimeBusy || isRuntimeLocked"
            title="清空输入框并保存自动发现模式"
            @click="resetToAutoDiscover"
          >
            使用自动发现
          </button>
        </div>

        <p v-if="isRuntimeLocked" class="hint" style="color: var(--warn)">
          服务运行中，禁止修改内核路径。如需切换请先停止服务。
        </p>
        <p v-if="runtimeHint" class="hint" style="color: var(--ok)">{{ runtimeHint }}</p>
      </template>
    </div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">连接配置</h2>
        <span class="spacer"></span>
        <button class="btn" @click="toggleConnection">
          {{ showConnection ? '收起' : '展开' }}
        </button>
      </div>

      <template v-if="showConnection && config">
        <div class="row" style="margin-top: 14px; align-items: flex-end">
          <label class="field">
            监听地址
            <input v-model="config.host" type="text" style="width: 140px" />
          </label>
          <label class="field">
            端口
            <input v-model.number="config.port" type="number" style="width: 100px" />
          </label>
          <label class="field">
            鉴权模式
            <select v-model="config.auth_mode">
              <option value="">默认（{{ config.effective_mode }}）</option>
              <option value="open">open</option>
              <option value="bearer">bearer</option>
              <option value="oauth">oauth</option>
              <option value="dual">dual</option>
            </select>
          </label>
        </div>

        <div class="row" style="margin-top: 12px; align-items: flex-end">
          <label class="field" style="flex: 1">
            Bearer Token
            <div class="token-input-wrap">
              <input
                v-model="config.token"
                :type="showToken ? 'text' : 'password'"
                autocomplete="off"
                spellcheck="false"
                placeholder="未设置"
                style="flex: 1"
              />
              <button
                type="button"
                class="btn"
                :title="showToken ? '切换为密文隐藏' : '显示明文 Token'"
                @click="showToken = !showToken"
              >
                {{ showToken ? '隐藏' : '显示' }}
              </button>
            </div>
          </label>
          <button class="btn" @click="generateToken">生成</button>
          <button class="btn primary" :disabled="busy" @click="saveConfig">保存</button>
        </div>

        <div v-if="config.auth_mode === 'oauth' || config.auth_mode === 'dual'" class="row" style="margin-top: 12px">
          <label class="field" style="flex: 1">
            OAuth server_url（公网 Origin）
            <input v-model="config.oauth_server_url" type="text" placeholder="https://mcp.example.com" />
          </label>
        </div>

        <p class="hint">
          配置将安全写入 config.yaml，自动保留注释与扩展字段，重启服务后生效。
        </p>
        <p v-if="savedHint" class="hint" style="color: var(--ok)">{{ savedHint }}</p>
      </template>
    </div>
  </div>
</template>
