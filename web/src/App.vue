<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { api, getToken, setToken } from './api'
import UpstreamsView from './components/UpstreamsView.vue'
import LogsView from './components/LogsView.vue'
import SettingsView from './components/SettingsView.vue'

// 左侧图标栏只保留三项：反代卡片、日志、设置。
// icon 是 SVG path 集合，避免为了几个图标引入整套图标库。
const tabs = [
  {
    id: 'upstreams',
    label: '以太链接',
    description: '管理 Audiobookshelf 与 Emby 的反代入口和 302 播放链路。',
    paths: [
      'M10 13a5 5 0 0 0 7.1 0l2-2a5 5 0 0 0-7.1-7.1l-1.1 1.2',
      'M14 11a5 5 0 0 0-7.1 0l-2 2A5 5 0 0 0 12 20.1l1.1-1.2',
      'M8.5 15.5l7-7'
    ]
  },
  {
    id: 'logs',
    label: '运行日志',
    description: '查看播放流水、302 命中情况与服务运行日志。',
    paths: ['M5 5h14v14H5z', 'M8 15l2-2 2 1 4-5', 'M8 9h.01']
  },
  {
    id: 'settings',
    label: '系统设置',
    description: '管理缓存、运行日志和登录账号。',
    paths: [
      'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
      'M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-2.9 1.2v.2a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-2.9-1.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.7 1.7 0 0 0 4 15H3.8a2 2 0 1 1 0-4h.3a1.7 1.7 0 0 0 1.2-2.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.7 1.7 0 0 0 11 4V3.8a2 2 0 1 1 4 0V4a1.7 1.7 0 0 0 2.9 1.2l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1A1.7 1.7 0 0 0 20 11h.2a2 2 0 1 1 0 4H20z'
    ]
  }
]

const RAIL_KEY = 'aetherlink.rail'
const APP_BASE = '/aetherlink/'
const TAB_IDS = new Set(tabs.map((tab) => tab.id))

function tabFromPath(pathname) {
  const suffix = pathname.replace(APP_BASE, '').split('/')[0]
  return TAB_IDS.has(suffix) ? suffix : 'upstreams'
}

function pathForTab(tab) {
  return `${APP_BASE}${tab}`
}

// gate 决定首屏：loading / login / app。没有初始化向导——首次启动就带内置账号。
const gate = ref('loading')
const activeTab = ref(tabFromPath(window.location.pathname))
// 侧栏展开状态记在 localStorage，刷新后保持上次的选择。
const railOpen = ref(localStorage.getItem(RAIL_KEY) === 'open')

const username = ref('')
const password = ref('')
const authBusy = ref(false)
const authError = ref('')

const status = ref(null)
const statusError = ref('')
let statusTimer = null

const activeLabel = computed(() => tabs.find((tab) => tab.id === activeTab.value)?.label || '')
const activeDescription = computed(() => tabs.find((tab) => tab.id === activeTab.value)?.description || '')

watch(railOpen, (open) => localStorage.setItem(RAIL_KEY, open ? 'open' : 'closed'))

function navigateTo(tab) {
  if (!TAB_IDS.has(tab) || activeTab.value === tab) return
  activeTab.value = tab
  window.history.pushState({ tab }, '', pathForTab(tab))
}

function handlePopState() {
  activeTab.value = tabFromPath(window.location.pathname)
}

async function bootstrap() {
  try {
    // 只是探一下后端在不在，拿不到就把原因直接显示在登录页上。
    await api.bootstrap()
  } catch (error) {
    authError.value = error.message
  }
  if (!getToken()) {
    gate.value = 'login'
    return
  }
  // 有旧令牌就先试一次，能用就直接进主界面。
  try {
    status.value = await api.status()
    enterApp()
  } catch {
    setToken('')
    gate.value = 'login'
  }
}

function enterApp() {
  gate.value = 'app'
  authError.value = ''
  password.value = ''
  if (statusTimer) clearInterval(statusTimer)
  statusTimer = setInterval(refreshStatus, 15000)
}

async function refreshStatus() {
  try {
    status.value = await api.status()
    statusError.value = ''
  } catch (error) {
    statusError.value = error.message
    if (error.status === 401) {
      setToken('')
      status.value = null
      gate.value = 'login'
    }
  }
}

async function submitLogin() {
  authError.value = ''
  authBusy.value = true
  try {
    const result = await api.login(username.value, password.value)
    setToken(result.token)
    status.value = await api.status()
    enterApp()
  } catch (error) {
    setToken('')
    authError.value = error.status === 401 ? '账号或密码不正确' : error.message
  } finally {
    authBusy.value = false
  }
}

async function logout() {
  try {
    await api.logout()
  } catch {
    // 令牌可能已经过期，本地清掉就够了。
  }
  setToken('')
  status.value = null
  if (statusTimer) clearInterval(statusTimer)
  gate.value = 'login'
}

async function onAccountChanged() {
  setToken('')
  status.value = null
  if (statusTimer) clearInterval(statusTimer)
  password.value = ''
  authError.value = '账号已更新，请重新登录'
  gate.value = 'login'
}
const uptime = computed(() => {
  const seconds = status.value?.uptimeSeconds || 0
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  return hours > 0 ? `${hours} 小时 ${minutes} 分` : `${minutes} 分`
})

onMounted(() => {
  if (window.location.pathname === APP_BASE || window.location.pathname === APP_BASE.slice(0, -1)) {
    window.history.replaceState({ tab: activeTab.value }, '', pathForTab(activeTab.value))
  }
  window.addEventListener('popstate', handlePopState)
  bootstrap()
})
onUnmounted(() => {
  window.removeEventListener('popstate', handlePopState)
  if (statusTimer) clearInterval(statusTimer)
})
</script>

<template>
  <div v-if="gate === 'loading'" class="gate">
    <div class="panel">
      <div class="logo">AL</div>
      <h2>AetherLink 以太链接</h2>
      <p class="muted">正在连接服务…</p>
    </div>
  </div>

  <div v-else-if="gate === 'login'" class="gate">
    <div class="panel">
      <div class="logo">AL</div>
      <h2>AetherLink</h2>
      <label class="field">
        <span>账号</span>
        <input v-model="username" autocomplete="username" @keyup.enter="submitLogin" />
      </label>
      <label class="field">
        <span>密码</span>
        <input v-model="password" type="password" autocomplete="current-password" @keyup.enter="submitLogin" />
      </label>
      <button class="primary block" :disabled="authBusy" @click="submitLogin">
        {{ authBusy ? '登录中…' : '登录' }}
      </button>
      <p v-if="authError" class="error">{{ authError }}</p>
    </div>
  </div>

  <div v-else class="shell" :class="{ 'rail-open': railOpen }">
    <nav class="rail" aria-label="主导航">
      <div class="rail-top">
        <div class="brand" aria-hidden="true">
          <svg viewBox="0 0 24 24">
            <path d="M8.5 15.5 15.5 8.5" />
            <path d="M10 13a4 4 0 0 0 5.7 0l2-2a4 4 0 0 0-5.7-5.7l-1 1" />
            <path d="M14 11a4 4 0 0 0-5.7 0l-2 2A4 4 0 0 0 12 18.7l1-1" />
          </svg>
        </div>
        <div class="rail-brand-copy">
          <strong>AetherLink</strong>
          <span>以太链接</span>
        </div>
      </div>

      <div class="rail-rule"></div>

      <div class="rail-nav">
        <button
          v-for="tab in tabs"
          :key="tab.id"
          :class="{ active: activeTab === tab.id }"
          :title="tab.label"
          :aria-label="tab.label"
          :aria-current="activeTab === tab.id ? 'page' : undefined"
          @click="navigateTo(tab.id)"
        >
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <path v-for="(path, index) in tab.paths" :key="index" :d="path" />
          </svg>
          <span class="rail-label">{{ tab.label }}</span>
        </button>
      </div>

      <div class="spacer"></div>

      <div class="rail-health" v-if="status">
        <span class="health-dot"></span>
        <span class="rail-label">服务运行中</span>
      </div>

      <button class="rail-logout" title="退出登录" aria-label="退出登录" @click="logout">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="M15 5H7a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h8" />
          <path d="M17 8l4 4-4 4" />
          <path d="M21 12h-8" />
        </svg>
        <span class="rail-label">退出登录</span>
      </button>

      <button
        class="rail-edge-toggle"
        :title="railOpen ? '收起侧栏' : '展开侧栏'"
        :aria-label="railOpen ? '收起侧栏' : '展开侧栏'"
        :aria-expanded="railOpen"
        @click="railOpen = !railOpen"
      >
        <span class="rail-edge-line"></span>
        <svg viewBox="0 0 16 24" aria-hidden="true">
          <path :d="railOpen ? 'M10 6 5 12l5 6' : 'm6 6 5 6-5 6'" />
        </svg>
      </button>
    </nav>

    <main class="main">
      <header class="page-head page-head-banner">
        <div class="page-title">
          <span class="eyebrow">AETHERLINK</span>
          <h1>{{ activeLabel }}</h1>
          <p>{{ activeDescription }}</p>
        </div>
        <div class="system-summary" v-if="status">
          <span class="status-pill online"><i></i>运行中</span>
          <span class="status-pill">v1.0</span>
          <template v-if="activeTab === 'settings'">
            <span class="status-pill">管理端口 {{ status.adminPort }}</span>
            <span class="status-pill">链接 {{ status.enabledUpstreamCount }}/{{ status.upstreamCount }}</span>
            <span class="status-pill">已运行 {{ uptime }}</span>
          </template>
        </div>
      </header>

      <p v-if="statusError" class="error" style="margin-top:0">{{ statusError }}</p>
      <div v-if="status?.restartRequired" class="notice">
        管理监听地址已改为 {{ status.listen }}，但进程仍在 {{ status.bootListen }} 上，需要重启容器才会生效。
      </div>
      <div v-if="status?.defaultCredentials" class="notice">
        仍在使用默认账号 admin / password，请到设置页修改。
      </div>

      <UpstreamsView v-if="activeTab === 'upstreams'" @changed="refreshStatus" />
      <LogsView v-else-if="activeTab === 'logs'" />
      <SettingsView v-else :status="status" @saved="refreshStatus" @account-changed="onAccountChanged" />
    </main>
  </div>
</template>
