<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { api, getToken, setToken, setSessionExpiredHandler, visibleMessage } from './api'
import UpstreamsView from './components/UpstreamsView.vue'
import LogsView from './components/LogsView.vue'
import SettingsView from './components/SettingsView.vue'

// 左侧图标栏只保留三项：反代卡片、日志、设置。
// icon 是 SVG path 集合，避免为了几个图标引入整套图标库。
const tabs = [
  {
    id: 'upstreams',
    label: '以太链接',
    description: '管理 Audiobookshelf、Emby 与飞牛影视的反代入口和 302 播放链路。',
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
const primaryTabs = tabs.filter((tab) => tab.id !== 'settings')
const settingsTab = tabs.find((tab) => tab.id === 'settings')

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
const accountMenuOpen = ref(false)

// 登录页就是每个浏览器各自输一次：账号框留空，不预填任何名字。
const username = ref('')
const password = ref('')
// 密码与上游编辑窗口同一套约定：默认就是一串圆点，右侧小眼睛点一下显示明文、
// 图标变成斜线眼睛，再点一下回到圆点。
const passwordVisible = ref(false)
// 「保持登录」勾选状态记在 localStorage：下次打开登录页还是上次的选择，省得重启
// 之后除了重登还要再勾一次。它只影响下一次登录，不改变已经签发出去的那个会话。
const REMEMBER_KEY = 'aetherlink.remember'
const rememberLogin = ref(localStorage.getItem(REMEMBER_KEY) === 'on')
watch(rememberLogin, (value) => {
  if (value) localStorage.setItem(REMEMBER_KEY, 'on')
  else localStorage.removeItem(REMEMBER_KEY)
})
const authBusy = ref(false)
const authError = ref('')

const status = ref(null)
const pageStats = ref(null)
const statusError = ref('')
let statusTimer = null

const activeLabel = computed(() => tabs.find((tab) => tab.id === activeTab.value)?.label || '')
const activeDescription = computed(() => tabs.find((tab) => tab.id === activeTab.value)?.description || '')

watch(railOpen, (open) => localStorage.setItem(RAIL_KEY, open ? 'open' : 'closed'))

function navigateTo(tab) {
  if (!TAB_IDS.has(tab) || activeTab.value === tab) return
  accountMenuOpen.value = false
  activeTab.value = tab
  // 统计值由页面组件回传，换页时先清空，避免顶栏短暂显示上一个页面的数字。
  pageStats.value = null
  window.history.pushState({ tab }, '', pathForTab(tab))
}

function handlePopState() {
  activeTab.value = tabFromPath(window.location.pathname)
  pageStats.value = null
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
    // 令牌已经失效（401 已由 api 层清掉令牌并切回登录页），或者后端暂时不可达：
    // 两种情况都停在登录页，并且不显示任何错误文案。
    setToken('')
    resetToLogin()
  }
}

function enterApp() {
  gate.value = 'app'
  // 清掉上一段会话残留的错误提示（比如容器重启导致的会话失效），
  // 不然重新登录后旧文案还会在页面顶部挂到下次轮询成功为止。
  statusError.value = ''
  authError.value = ''
  password.value = ''
  // 密码框回到默认的圆点状态，不让上一次手动显示明文的选择留到下一次登录。
  passwordVisible.value = false
  if (statusTimer) clearInterval(statusTimer)
  statusTimer = setInterval(refreshStatus, 15000)
}

async function refreshStatus() {
  try {
    status.value = await api.status()
    statusError.value = ''
  } catch (error) {
    // 会话失效（401）已经由 api 层统一处理成「清令牌 + 回登录页」，这里不再重复
    // 判断，也不要把后端那句「会话无效或已过期，请重新登录」挂到界面上。
    statusError.value = visibleMessage(error)
  }
}

async function submitLogin() {
  authError.value = ''
  authBusy.value = true
  try {
    const result = await api.login(username.value, password.value, rememberLogin.value)
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

// resetToLogin 把界面退回登录页：停掉状态轮询、丢掉缓存的状态。会话失效、主动
// 退出、改完账号都走这里，区别只在要不要在登录页留一条说明文案。
function resetToLogin() {
  accountMenuOpen.value = false
  status.value = null
  statusError.value = ''
  if (statusTimer) {
    clearInterval(statusTimer)
    statusTimer = null
  }
  gate.value = 'login'
}

// 令牌失效由 api 层统一上报（见 api.js 的 request）。这里只把界面收干净：不显示
// 任何错误文案，直接回登录页——容器重启后用户看到的就是干净的登录页，而不是一句
// 「会话无效或已过期，请重新登录」。
setSessionExpiredHandler(() => {
  authError.value = ''
  resetToLogin()
})

async function logout() {
  accountMenuOpen.value = false
  try {
    await api.logout()
  } catch {
    // 令牌可能已经过期，本地清掉就够了。
  }
  setToken('')
  resetToLogin()
}

async function onAccountChanged() {
  setToken('')
  password.value = ''
  authError.value = '账号已更新，请重新登录'
  resetToLogin()
}
const uptime = computed(() => {
  const seconds = status.value?.uptimeSeconds || 0
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  return hours > 0 ? `${hours} 小时 ${minutes} 分` : `${minutes} 分`
})

// 顶栏右侧的指标块。每项自带图标与配色，值由当前页面的组件通过 @stats 回传，
// 页面里不再单独排一行统计条。
const headerStats = computed(() => {
  const stats = pageStats.value
  if (!stats) return []

  if (activeTab.value === 'upstreams') {
    return [
      { key: 'total', label: '总链接', value: stats.total, tone: 'violet', paths: ['M5 7h14M5 12h14M5 17h9'] },
      {
        key: 'emby',
        label: 'Emby',
        value: stats.emby,
        tone: 'blue',
        paths: ['M7 5h10a3 3 0 0 1 3 3v8a3 3 0 0 1-3 3H7a3 3 0 0 1-3-3V8a3 3 0 0 1 3-3z', 'M8 9h8M8 13h5']
      },
      {
        key: 'fnos',
        label: '飞牛影视',
        value: stats.fnos,
        tone: 'indigo',
        paths: ['M3.5 6h17v12h-17z', 'M8 6v12M16 6v12', 'M3.5 12h4.5M16 12h4.5']
      },
      { key: 'abs', label: 'ABS', value: stats.abs, tone: 'amber', paths: ['M6 5h12v14H6z', 'M9 8h6M9 12h6M9 16h4'] },
      { key: 'running', label: '正在运行', value: stats.running, tone: 'green', paths: ['m5 12 4 4L19 6'] },
      { key: 'stopped', label: '停止运行', value: stats.stopped, tone: 'rose', paths: ['M9 6v12M15 6v12'] }
    ]
  }

  if (activeTab.value === 'logs') {
    return [
      {
        key: 'requests',
        label: '播放请求',
        value: stats.requests,
        tone: 'violet',
        paths: ['M4 7h16M4 12h10M4 17h7', 'M21 16a3 3 0 1 1-6 0 3 3 0 0 1 6 0']
      },
      { key: 'redirects', label: '302 跳转', value: stats.redirects, tone: 'amber', paths: ['M5 12h13M13 6l6 6-6 6', 'M4 5v14'] },
      {
        key: 'relay',
        label: '中继/转码',
        value: stats.relay,
        tone: 'blue',
        paths: ['M4 12h16M12 4v16', 'M20 12a8 8 0 1 1-16 0 8 8 0 0 1 16 0']
      },
      { key: 'errors', label: '失败', value: stats.errors, tone: 'rose', paths: ['M12 4 21 20H3z', 'M12 9.5v5', 'M12 17.5h.01'] },
      { key: 'logLines', label: '服务日志', value: stats.logLines, tone: 'green', paths: ['M5 5h14v14H5z', 'M8 9h8M8 13h6M8 17h4'] }
    ]
  }

  return []
})

onMounted(() => {
  if (window.location.pathname === APP_BASE || window.location.pathname === APP_BASE.slice(0, -1)) {
    window.history.replaceState({ tab: activeTab.value }, '', pathForTab(activeTab.value))
  }
  window.addEventListener('popstate', handlePopState)
  document.addEventListener('click', closeAccountMenu)
  bootstrap()
})
onUnmounted(() => {
  window.removeEventListener('popstate', handlePopState)
  document.removeEventListener('click', closeAccountMenu)
  if (statusTimer) clearInterval(statusTimer)
})

function closeAccountMenu() {
  accountMenuOpen.value = false
}

function toggleAccountMenu() {
  accountMenuOpen.value = !accountMenuOpen.value
}
</script>

<template>
  <div v-if="gate === 'loading'" class="gate">
    <div class="panel">
      <img class="logo" :src="'/aetherlink/icons/aetherlink-logo.png'" alt="AetherLink" />
      <h2>AetherLink 以太链接</h2>
      <p class="muted">正在连接服务…</p>
    </div>
  </div>

  <div v-else-if="gate === 'login'" class="gate">
    <div class="panel">
      <img class="logo" :src="'/aetherlink/icons/aetherlink-logo.png'" alt="AetherLink" />
      <h2>AetherLink</h2>
      <label class="field">
        <span>账号</span>
        <input v-model="username" autocomplete="username" @keyup.enter="submitLogin" />
      </label>
      <label class="field">
        <span>密码</span>
        <span class="secret-input">
          <input
            v-model="password"
            :type="passwordVisible ? 'text' : 'password'"
            autocomplete="current-password"
            @keyup.enter="submitLogin"
          />
          <button
            type="button"
            class="secret-toggle"
            :aria-pressed="passwordVisible"
            :title="passwordVisible ? '隐藏密码' : '显示密码'"
            :aria-label="passwordVisible ? '隐藏密码' : '显示密码'"
            @click.prevent="passwordVisible = !passwordVisible"
          >
            <svg v-if="passwordVisible" viewBox="0 0 24 24" aria-hidden="true">
              <path d="M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z" />
              <circle cx="12" cy="12" r="3.1" />
              <path d="m4 3.6 16 16.8" />
            </svg>
            <svg v-else viewBox="0 0 24 24" aria-hidden="true">
              <path d="M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z" />
              <circle cx="12" cy="12" r="3.1" />
            </svg>
          </button>
        </span>
      </label>
      <label class="inline login-remember">
        <input type="checkbox" v-model="rememberLogin" />
        <span>保持登录</span>
      </label>
      <button class="primary block" :disabled="authBusy" @click="submitLogin">
        {{ authBusy ? '登录中…' : '登录' }}
      </button>
      <p v-if="authError" class="error">{{ authError }}</p>
    </div>
  </div>

  <div v-else class="shell" :class="{ 'rail-open': railOpen }">
    <nav class="rail" aria-label="主导航">
      <div class="rail-top rail-account" @click.stop>
        <button
          class="brand"
          type="button"
          aria-label="打开账号菜单"
          :aria-expanded="accountMenuOpen"
          @click="toggleAccountMenu"
        >
          <img :src="'/aetherlink/icons/aetherlink-logo.png'" alt="AetherLink" />
        </button>
        <div class="rail-brand-copy">
          <strong>AetherLink</strong>
          <span>以太链接</span>
        </div>
        <div v-if="accountMenuOpen" class="rail-account-menu">
          <button class="rail-logout-button" type="button" aria-label="退出登录" @click="logout">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M15 5H7a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h8" />
              <path d="M17 8l4 4-4 4" />
              <path d="M21 12h-8" />
            </svg>
          </button>
        </div>
      </div>

      <div class="rail-rule"></div>

      <div class="rail-nav">
        <button
          type="button"
          :aria-label="railOpen ? '折叠栏目' : '展开栏目'"
          :aria-expanded="railOpen"
          @click="railOpen = !railOpen"
        >
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <path d="M4 6h16M4 12h16M4 18h16" />
          </svg>
          <span class="rail-label">折叠栏目</span>
        </button>
        <button
          v-for="tab in primaryTabs"
          :key="tab.id"
          :class="{ active: activeTab === tab.id }"
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

      <div class="rail-nav rail-nav-bottom">
        <button
          v-if="settingsTab"
          :class="{ active: activeTab === settingsTab.id }"
          :aria-label="settingsTab.label"
          :aria-current="activeTab === settingsTab.id ? 'page' : undefined"
          @click="navigateTo(settingsTab.id)"
        >
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <path v-for="(path, index) in settingsTab.paths" :key="index" :d="path" />
          </svg>
          <span class="rail-label">{{ settingsTab.label }}</span>
        </button>
      </div>

    </nav>

    <main class="main">
      <header class="page-head page-head-banner">
        <div class="page-title">
          <span class="eyebrow">AETHERLINK</span>
          <h1>{{ activeLabel }}</h1>
          <p>{{ activeDescription }}</p>
        </div>
        <div class="system-summary" v-if="headerStats.length">
          <span v-for="stat in headerStats" :key="stat.key" class="header-stat">
            <span class="header-stat-icon" :class="stat.tone" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <path v-for="(path, index) in stat.paths" :key="index" :d="path" />
              </svg>
            </span>
            <span class="header-stat-text">
              <small>{{ stat.label }}</small>
              <b>{{ stat.value }}</b>
            </span>
          </span>
        </div>
      </header>

      <p v-if="statusError" class="error" style="margin-top:0">{{ statusError }}</p>
      <div v-if="status?.restartRequired" class="notice">
        管理监听地址已改为 {{ status.listen }}，但进程仍在 {{ status.bootListen }} 上，需要重启容器才会生效。
      </div>
      <div v-if="status?.defaultCredentials" class="notice">
        仍在使用默认账号 admin / password，请到设置页修改。
      </div>

      <UpstreamsView v-if="activeTab === 'upstreams'" @changed="refreshStatus" @stats="pageStats = $event" />
      <LogsView v-else-if="activeTab === 'logs'" @stats="pageStats = $event" />
      <SettingsView v-else :status="status" @saved="refreshStatus" @account-changed="onAccountChanged" />
    </main>
  </div>
</template>
