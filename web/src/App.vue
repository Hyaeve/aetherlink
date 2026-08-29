<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api, getToken, setToken } from './api'
import DashboardView from './components/DashboardView.vue'
import UpstreamsView from './components/UpstreamsView.vue'
import LibraryView from './components/LibraryView.vue'
import StrmLabView from './components/StrmLabView.vue'
import LogsView from './components/LogsView.vue'
import SettingsView from './components/SettingsView.vue'

// 左侧图标栏。icon 是 SVG path 集合，避免引入图标库多一份依赖。
const tabs = [
  {
    id: 'upstreams',
    label: '反代上游',
    paths: ['M4 7a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z']
  },
  {
    id: 'dashboard',
    label: '概览',
    paths: ['M4 4h7v7H4z', 'M13 4h7v4h-7z', 'M13 10h7v10h-7z', 'M4 13h7v7H4z']
  },
  {
    id: 'library',
    label: '书库浏览',
    paths: ['M5 5h5v14H5z', 'M12 5h3v14h-3z', 'M17 6l3 12']
  },
  {
    id: 'strm',
    label: 'STRM 实验室',
    paths: ['M10 4v5l-5 8a2 2 0 0 0 1.7 3h10.6A2 2 0 0 0 19 17l-5-8V4z', 'M9 4h6']
  },
  {
    id: 'logs',
    label: '日志',
    paths: ['M6 4h9l3 3v13H6z', 'M9 10h7', 'M9 14h7']
  },
  {
    id: 'settings',
    label: '设置',
    paths: [
      'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
      'M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-2.9 1.2v.2a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-2.9-1.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.7 1.7 0 0 0 4 15H3.8a2 2 0 1 1 0-4h.3a1.7 1.7 0 0 0 1.2-2.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.7 1.7 0 0 0 11 4V3.8a2 2 0 1 1 4 0V4a1.7 1.7 0 0 0 2.9 1.2l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1A1.7 1.7 0 0 0 20 11h.2a2 2 0 1 1 0 4H20z'
    ]
  }
]

// gate 决定首屏：loading / login / app。没有初始化向导——首次启动就带内置账号。
const gate = ref('loading')
const activeTab = ref('upstreams')

const username = ref('')
const password = ref('')
const authBusy = ref(false)
const authError = ref('')

const status = ref(null)
const statusError = ref('')
let statusTimer = null

const activeLabel = computed(() => tabs.find((tab) => tab.id === activeTab.value)?.label || '')

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

onMounted(bootstrap)
onUnmounted(() => statusTimer && clearInterval(statusTimer))
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

  <div v-else class="shell">
    <nav class="rail">
      <div class="brand">AL</div>
      <button
        v-for="tab in tabs"
        :key="tab.id"
        :class="{ active: activeTab === tab.id }"
        :title="tab.label"
        :aria-label="tab.label"
        :aria-current="activeTab === tab.id ? 'page' : undefined"
        @click="activeTab = tab.id"
      >
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path v-for="(path, index) in tab.paths" :key="index" :d="path" />
        </svg>
      </button>
      <div class="spacer"></div>
      <button title="退出登录" aria-label="退出登录" @click="logout">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="M15 5H7a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h8" />
          <path d="M17 8l4 4-4 4" />
          <path d="M21 12h-8" />
        </svg>
      </button>
    </nav>

    <main class="main">
      <div class="page-head">
        <h1>{{ activeLabel }}</h1>
        <span class="sub" v-if="status">
          v{{ status.version }} · 监听 {{ status.listen }} · 302 模式 {{ status.redirectMode }} ·
          上游 {{ status.enabledUpstreamCount }}/{{ status.upstreamCount }} · 运行 {{ uptime }}
        </span>
      </div>

      <p v-if="statusError" class="error" style="margin-top:0">{{ statusError }}</p>
      <div v-if="status?.restartRequired" class="notice">
        监听地址已改为 {{ status.listen }}，但进程仍在 {{ status.bootListen }} 上，需要重启容器才会生效。
      </div>
      <div v-if="status?.defaultCredentials" class="notice">
        仍在使用默认账号 admin / password，请到设置页修改。
      </div>

      <UpstreamsView v-if="activeTab === 'upstreams'" @changed="refreshStatus" />
      <DashboardView v-else-if="activeTab === 'dashboard'" :status="status" @refresh-status="refreshStatus" />
      <LibraryView v-else-if="activeTab === 'library'" />
      <StrmLabView v-else-if="activeTab === 'strm'" />
      <LogsView v-else-if="activeTab === 'logs'" />
      <SettingsView v-else :status="status" @saved="refreshStatus" @account-changed="onAccountChanged" />
    </main>
  </div>
</template>
