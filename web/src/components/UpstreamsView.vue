<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'
import { cardStyleFor } from '../palette'
import ContextMenu from './ContextMenu.vue'
import UpstreamForm from './UpstreamForm.vue'

const emit = defineEmits(['changed'])

const upstreams = ref([])
const suggestedPort = ref(0)
const adminPort = ref(0)
const error = ref('')
const notice = ref('')
const loading = ref(true)

// editing: null 不打开；{ upstream: null } 新增；{ upstream } 编辑。
const editing = ref(null)
const menu = ref(null)
const pendingDelete = ref(null)
const busy = ref(false)

const TYPE_LABELS = { audiobookshelf: 'Audiobookshelf', emby: 'Emby' }

async function load() {
  loading.value = true
  try {
    const payload = await api.upstreams()
    upstreams.value = payload.upstreams || []
    suggestedPort.value = payload.suggestedPort || 0
    adminPort.value = payload.adminPort || 0
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
  } finally {
    loading.value = false
  }
}

// 卡片上只显示「主机:端口」，完整地址留给详细编辑窗口。
function hostLabel(baseUrl) {
  try {
    const parsed = new URL(baseUrl)
    return parsed.port ? `${parsed.hostname}:${parsed.port}` : parsed.hostname
  } catch {
    return baseUrl
  }
}

// 上游端口 → 反代端口，播放端只要把地址里的端口换成后者。
function portFlow(upstream) {
  let source = ''
  try {
    const parsed = new URL(upstream.baseUrl)
    source = parsed.port || (parsed.protocol === 'https:' ? '443' : '80')
  } catch {
    source = '?'
  }
  return `${source} → ${upstream.listenPort}`
}

function typeLabel(type) {
  return TYPE_LABELS[type] || type
}

function redirectLabel(mode) {
  return { always: '始终跳转', public: '公网跳转', private: '内网跳转', never: '始终中继' }[mode] || '始终跳转'
}

function openProxy(upstream) {
  if (!upstream.enabled || !upstream.listening || !upstream.listenPort) return
  const target = `${window.location.protocol}//${window.location.hostname}:${upstream.listenPort}`
  window.open(target, '_blank', 'noopener,noreferrer')
}

function openMenu(event, upstream) {
  menu.value = { x: event.clientX, y: event.clientY, upstream }
}

function closeMenu() {
  menu.value = null
}

function openEditor(upstream) {
  closeMenu()
  notice.value = ''
  editing.value = { upstream: upstream || null }
}

async function onSaved() {
  editing.value = null
  await load()
  emit('changed')
}

async function toggleEnabled(upstream) {
  closeMenu()
  busy.value = true
  try {
    await api.updateUpstream(upstream.name, { enabled: !upstream.enabled })
    await load()
    emit('changed')
  } catch (toggleError) {
    error.value = toggleError.message
  } finally {
    busy.value = false
  }
}

async function ping(upstream) {
  closeMenu()
  notice.value = `正在测试 ${upstream.name}…`
  try {
    const result = await api.ping(upstream.name)
    notice.value = `${upstream.name} 连接正常 · ${result.info}`
  } catch (pingError) {
    notice.value = `${upstream.name} 连接失败：${pingError.message}`
  }
}

function askDelete(upstream) {
  closeMenu()
  pendingDelete.value = upstream
}

async function confirmDelete() {
  const target = pendingDelete.value
  busy.value = true
  try {
    await api.deleteUpstream(target.name)
    pendingDelete.value = null
    await load()
    emit('changed')
  } catch (deleteError) {
    error.value = deleteError.message
    pendingDelete.value = null
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="upstreams-page">
    <p v-if="error" class="error page-error">{{ error }}</p>
    <div v-if="notice" class="notice page-notice">{{ notice }}</div>

    <div class="overview-strip">
      <div class="overview-item">
        <span class="overview-icon violet"><svg viewBox="0 0 24 24"><path d="M5 7h14M5 12h14M5 17h9" /></svg></span>
        <span><small>已配置链接</small><strong>{{ upstreams.length }}</strong></span>
      </div>
      <div class="overview-item">
        <span class="overview-icon green"><svg viewBox="0 0 24 24"><path d="m5 12 4 4L19 6" /></svg></span>
        <span><small>正在运行</small><strong>{{ upstreams.filter((item) => item.enabled && item.listening).length }}</strong></span>
      </div>
      <div class="overview-item">
        <span class="overview-icon blue"><svg viewBox="0 0 24 24"><path d="M4 12h16M12 4v16" /></svg></span>
        <span><small>下一个端口</small><strong>{{ suggestedPort || '—' }}</strong></span>
      </div>
    </div>

    <div v-if="loading" class="card-grid">
      <div v-for="index in 2" :key="index" class="proxy-card skeleton-card" aria-hidden="true">
        <span class="skeleton-line short"></span>
        <span class="skeleton-line medium"></span>
        <span class="skeleton-line long"></span>
        <span class="skeleton-line chips"></span>
      </div>
    </div>

    <div v-else-if="upstreams.length" class="card-grid">
      <article
        v-for="upstream in upstreams"
        :key="upstream.name"
        class="proxy-card"
        :class="{ dimmed: !upstream.enabled }"
        :style="cardStyleFor(upstream.name)"
        tabindex="0"
        role="button"
        :aria-label="`编辑 ${upstream.name}`"
        @click="openEditor(upstream)"
        @keyup.enter="openEditor(upstream)"
        @keyup.space.prevent="openEditor(upstream)"
        @contextmenu.prevent="openMenu($event, upstream)"
      >
        <div class="proxy-card-top">
          <span class="service-mark" :class="upstream.type">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path v-if="upstream.type === 'emby'" d="m12 4 7 4v8l-7 4-7-4V8zM8 10l4 2 4-2M12 12v8" />
              <path v-else d="M6 5h8l4 4v10H6zM9 5v5h6M9 15h6M9 18h4" />
            </svg>
          </span>
          <span class="proxy-status" :class="upstream.enabled && upstream.listening ? 'online' : 'offline'">
            <i></i>{{ upstream.enabled && upstream.listening ? '运行中' : '未运行' }}
          </span>
        </div>

        <div class="proxy-card-content">
          <h3>{{ upstream.name }}</h3>
          <p class="proxy-type">{{ typeLabel(upstream.type) }}</p>
          <p class="proxy-host">{{ hostLabel(upstream.baseUrl) }}</p>
        </div>

        <div class="route-flow">
          <span>原端口</span>
          <strong>{{ portFlow(upstream).split(' → ')[0] }}</strong>
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h14M14 7l5 5-5 5" /></svg>
          <span>反代端口</span>
          <button
            class="route-port"
            :disabled="!upstream.enabled || !upstream.listening"
            :title="upstream.enabled && upstream.listening ? '打开反代入口' : '入口未运行'"
            @click.stop="openProxy(upstream)"
          >{{ upstream.listenPort }}</button>
        </div>

        <div class="proxy-card-foot">
          <span class="card-tag">{{ redirectLabel(upstream.redirectMode) }}</span>
          <span class="card-tag" :class="{ muted: !upstream.hasApiKey }">{{ upstream.hasApiKey ? '密钥已配置' : '缺少密钥' }}</span>
        </div>
      </article>

      <button class="card-add" @click="openEditor(null)">
        <span class="plus"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg></span>
        <strong>添加链接</strong>
        <small>接入新的媒体服务</small>
      </button>
    </div>

    <div v-if="!loading && !upstreams.length" class="empty-state">
      <span class="empty-orb"><svg viewBox="0 0 24 24"><path d="M5 12a7 7 0 0 1 12-5M19 12a7 7 0 0 1-12 5" /><path d="m15 5 2 2-2 2M9 19l-2-2 2-2" /></svg></span>
      <strong>还没有以太链接</strong>
      <p>添加 Audiobookshelf 或 Emby 后，AetherLink 会为它建立独立反代入口。</p>
      <button class="primary" @click="openEditor(null)">添加第一条链接</button>
    </div>

    <ContextMenu
      v-if="menu"
      :x="menu.x"
      :y="menu.y"
      :title="menu.upstream.name"
      @close="closeMenu"
    >
      <button @click="openEditor(menu.upstream)">详细编辑…</button>
      <button @click="ping(menu.upstream)">测试连接</button>
      <button :disabled="busy" @click="toggleEnabled(menu.upstream)">
        {{ menu.upstream.enabled ? '停用' : '启用' }}
      </button>
      <div class="divider"></div>
      <button class="danger" @click="askDelete(menu.upstream)">删除</button>
    </ContextMenu>

    <UpstreamForm
      v-if="editing"
      :upstream="editing.upstream"
      :suggested-port="suggestedPort"
      :admin-port="adminPort"
      @close="editing = null"
      @saved="onSaved"
    />

    <div v-if="pendingDelete" class="modal-backdrop" @click.self="pendingDelete = null">
      <div class="modal" style="width:min(420px,100%)">
        <div class="modal-head"><h2>删除上游</h2></div>
        <div class="modal-body">
          <p style="margin:0;font-size:13.5px;line-height:1.6">
            确认删除 <strong>{{ pendingDelete.name }}</strong>？
            它的地址与 API 密钥会一并从配置里移除，端口 {{ pendingDelete.listenPort }} 会立即停止监听。
          </p>
        </div>
        <div class="modal-foot">
          <div class="right">
            <button @click="pendingDelete = null">取消</button>
            <button class="primary" :disabled="busy" @click="confirmDelete">{{ busy ? '删除中…' : '确认删除' }}</button>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>
