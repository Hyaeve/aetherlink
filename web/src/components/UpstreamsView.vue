<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import { api, visibleMessage } from '../api'
import { cardStyleFor } from '../palette'
import ContextMenu from './ContextMenu.vue'
import FloatToast from './FloatToast.vue'
import UpstreamForm from './UpstreamForm.vue'

const emit = defineEmits(['changed', 'stats'])

const upstreams = ref([])
const suggestedPort = ref(0)
const error = ref('')
// toast：右键菜单里「测试」的结果。浮在页面上的毛玻璃提示，亮完自己隐去，
// 不占布局——页面顶部的横栏会把下面的卡片顶下去，只想知道结果时太吵。
const toast = ref(null)
const loading = ref(true)
const startedAt = ref('')

// editing: null 不打开；{ upstream: null } 新增；{ upstream } 编辑。
const editing = ref(null)
const menu = ref(null)
const pendingDelete = ref(null)
const busy = ref(false)

// 卡片右上角的跳转模式下拉：modeMenu 记录锚点与当前值，modePanel 用于挂载后回折定位。
const modeMenu = ref(null)
const modePanel = ref(null)
const modeBusy = ref(false)

const TYPE_LABELS = { audiobookshelf: 'Audiobookshelf', emby: 'Emby', fnos: '飞牛影视' }
// 卡片左上角的服务标识图，与 public/icons 下的文件名一一对应。
const TYPE_ICONS = { audiobookshelf: 'abs.png', emby: 'emby.png', fnos: 'fnmovie.png' }

// 与 config.RedirectMode 的四个取值一一对应（内网/公网按客户端 IP 判断，
// 见设置页「网络地址」里的前置代理与内网网段）。
const REDIRECT_OPTIONS = [
  { value: 'always', label: '始终跳转' },
  { value: 'public', label: '公网跳转' },
  { value: 'private', label: '内网跳转' },
  { value: 'never', label: '始终中继' }
]
const REDIRECT_LABELS = Object.fromEntries(REDIRECT_OPTIONS.map((option) => [option.value, option.label]))

const runningCount = computed(() => upstreams.value.filter((item) => item.enabled && item.listening).length)
const stoppedCount = computed(() => upstreams.value.length - runningCount.value)
// Emby 与飞牛影视同属 Emby 方言，统计上仍分开计数，方便一眼看清各自接了几个。
const embyCount = computed(() => upstreams.value.filter((item) => item.type === 'emby').length)
const fnosCount = computed(() => upstreams.value.filter((item) => item.type === 'fnos').length)
const absCount = computed(() => upstreams.value.filter((item) => item.type === 'audiobookshelf').length)

async function load() {
  loading.value = true
  try {
    const [payload, status] = await Promise.all([api.upstreams(), api.status()])
    startedAt.value = status.startedAt
    upstreams.value = payload.upstreams || []
    emit('stats', { total: upstreams.value.length, emby: embyCount.value, fnos: fnosCount.value, abs: absCount.value, running: runningCount.value, stopped: stoppedCount.value })
    suggestedPort.value = payload.suggestedPort || 0
    error.value = ''
  } catch (loadError) {
    // 会话失效（容器重启等）已由 api 层处理成回登录页，这里不显示任何文案。
    error.value = visibleMessage(loadError)
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

function typeIcon(type) {
  return `/aetherlink/icons/${TYPE_ICONS[type] || 'aetherlink-logo.png'}`
}

function redirectLabel(mode) {
  return REDIRECT_LABELS[mode] || REDIRECT_LABELS.always
}

function openProxy(upstream) {
  if (!upstream.enabled || !upstream.listening || !upstream.listenPort) return
  const target = `${window.location.protocol}//${window.location.hostname}:${upstream.listenPort}`
  window.open(target, '_blank', 'noopener,noreferrer')
}

function openMenu(event, upstream) {
  closeModeMenu()
  menu.value = { x: event.clientX, y: event.clientY, upstream }
}

function closeMenu() {
  menu.value = null
}

function closeModeMenu() {
  modeMenu.value = null
}

// 下拉挂在卡片外层（卡片 overflow:hidden 会裁掉绝对定位的子元素），
// 所以用 fixed 定位 + 挂载后按实际尺寸回折，贴住触发按钮的右下角。
// 宽度直接取触发按钮的宽度，和卡片右上角的模式标识一样宽。
async function openModeMenu(event, upstream) {
  if (busy.value || modeBusy.value) return
  const anchor = event.currentTarget.getBoundingClientRect()
  modeMenu.value = {
    name: upstream.name,
    current: upstream.redirectMode || 'always',
    width: Math.round(anchor.width),
    left: anchor.right,
    top: anchor.bottom + 8
  }
  await nextTick()
  const node = modePanel.value
  if (!node || !modeMenu.value) return
  const { width, height } = node.getBoundingClientRect()
  const margin = 10
  const below = anchor.bottom + 8
  modeMenu.value = {
    ...modeMenu.value,
    left: Math.min(Math.max(margin, anchor.right - width), Math.max(margin, window.innerWidth - width - margin)),
    // 下方放不下就翻到按钮上方，免得贴着视口底边被裁掉。
    top: below + height <= window.innerHeight - margin ? below : Math.max(margin, anchor.top - height - 8)
  }
}

async function selectMode(mode) {
  const target = modeMenu.value
  if (!target) return
  if (target.current === mode) {
    closeModeMenu()
    return
  }
  modeBusy.value = true
  busy.value = true
  try {
    await api.updateUpstream(target.name, { redirectMode: mode })
    closeModeMenu()
    // 切换结果直接体现在卡片上的模式标识里，不再额外弹一条页面横栏。
    await load()
    emit('changed')
  } catch (modeError) {
    error.value = visibleMessage(modeError)
  } finally {
    modeBusy.value = false
    busy.value = false
  }
}

// 面板是 fixed 定位，页面一滚动就会和卡片脱开，直接关掉更干净。
function onModeViewportChange() {
  if (modeMenu.value) closeModeMenu()
}

function onModeKeydown(event) {
  if (event.key === 'Escape') closeModeMenu()
}

// 悬浮提示同时只留一条：新的一条直接替换旧的，不会堆成一摞。
// 停留时长交给 FloatToast 自己计时（loading 态不计时，一直挂到结果回来）。
function showToast(text, options = {}) {
  toast.value = {
    text,
    tone: options.tone || 'info',
    loading: Boolean(options.loading),
    duration: options.duration || 4000
  }
}

function hideToast() {
  toast.value = null
}

function openEditor(upstream) {
  closeMenu()
  hideToast()
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
    error.value = visibleMessage(toggleError)
  } finally {
    busy.value = false
  }
}

async function ping(upstream) {
  closeMenu()
  showToast(`正在测试 ${upstream.name}…`, { loading: true })
  try {
    const result = await api.ping(upstream.name)
    showToast(`${upstream.name} 连接正常 · ${result.info}`, { tone: 'ok' })
  } catch (pingError) {
    // 会话失效已经回登录页了，别再弹一条「连接失败」出来误导人。
    if (pingError.sessionExpired) return
    // 失败信息通常更长，多留一会儿再隐去。
    showToast(`${upstream.name} 连接失败：${pingError.message}`, { tone: 'danger', duration: 6000 })
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
    error.value = visibleMessage(deleteError)
    pendingDelete.value = null
  } finally {
    busy.value = false
  }
}

onMounted(() => {
  load()
  window.addEventListener('resize', onModeViewportChange)
  window.addEventListener('scroll', onModeViewportChange, true)
  window.addEventListener('keydown', onModeKeydown)
})

onUnmounted(() => {
  window.removeEventListener('resize', onModeViewportChange)
  window.removeEventListener('scroll', onModeViewportChange, true)
  window.removeEventListener('keydown', onModeKeydown)
})
</script>

<template>
  <section class="upstreams-page">
    <p v-if="error" class="error page-error">{{ error }}</p>

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
        :style="cardStyleFor(upstream.name, startedAt)"
        tabindex="0"
        role="button"
        :aria-label="`${upstream.name}，点击卡片编辑`"
        @click="openEditor(upstream)"
        @keyup.enter="openEditor(upstream)"
        @keyup.space.prevent="openEditor(upstream)"
        @contextmenu.prevent="openMenu($event, upstream)"
      >
        <div class="proxy-card-top">
          <button
            type="button"
            class="service-mark"
            :class="upstream.type"
            :title="upstream.enabled ? '点击停用' : '点击启用'"
            :aria-label="`${upstream.name}，${upstream.enabled ? '点击停用' : '点击启用'}`"
            :aria-pressed="upstream.enabled"
            :disabled="busy"
            @click.stop="toggleEnabled(upstream)"
            @keyup.stop
          >
            <img :src="typeIcon(upstream.type)" :alt="typeLabel(upstream.type)" />
          </button>
          <button
            type="button"
            class="card-tag mode-tag mode-trigger"
            :class="{ open: modeMenu?.name === upstream.name }"
            :disabled="busy"
            :title="`播放跳转：${redirectLabel(upstream.redirectMode)}，点击切换`"
            :aria-label="`${upstream.name}，播放跳转 ${redirectLabel(upstream.redirectMode)}，点击切换`"
            :aria-expanded="modeMenu?.name === upstream.name"
            aria-haspopup="menu"
            @click.stop="openModeMenu($event, upstream)"
            @keyup.stop
          >
            <span>{{ redirectLabel(upstream.redirectMode) }}</span>
            <svg class="mode-caret" viewBox="0 0 24 24" aria-hidden="true">
              <path d="m6 9 6 6 6-6" />
            </svg>
          </button>
        </div>

        <div class="proxy-card-content">
          <h3>{{ upstream.name }}</h3>
          <p class="proxy-type">{{ typeLabel(upstream.type) }}</p>
          <p class="proxy-host">{{ hostLabel(upstream.baseUrl) }}</p>
        </div>

        <div class="route-flow">
          <div class="route-endpoint">
            <span>原端口</span>
            <strong>{{ portFlow(upstream).split(' → ')[0] }}</strong>
          </div>
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h14M14 7l5 5-5 5" /></svg>
          <div class="route-endpoint route-target">
            <span>反代端口</span>
          <button
            class="route-port"
            :disabled="!upstream.enabled || !upstream.listening"
            :title="upstream.enabled && upstream.listening ? '打开反代入口' : '入口未运行'"
            @click.stop="openProxy(upstream)"
          >{{ upstream.listenPort }}</button>
          </div>
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
      <p>添加 Audiobookshelf、Emby 或飞牛影视后，AetherLink 会为它建立独立反代入口。</p>
      <button class="primary" @click="openEditor(null)">添加第一条链接</button>
    </div>

    <!-- 右上角跳转模式下拉：透明遮罩兜住任意点击，保证一定能关掉。 -->
    <div
      v-if="modeMenu"
      class="mode-scrim"
      @click="closeModeMenu"
      @contextmenu.prevent="closeModeMenu"
      @wheel="closeModeMenu"
    >
      <div
        ref="modePanel"
        class="mode-menu"
        role="menu"
        :style="{ left: `${modeMenu.left}px`, top: `${modeMenu.top}px`, width: `${modeMenu.width}px` }"
        @click.stop
        @contextmenu.prevent.stop
      >
        <button
          v-for="option in REDIRECT_OPTIONS"
          :key="option.value"
          type="button"
          role="menuitemradio"
          class="mode-option"
          :class="{ selected: modeMenu.current === option.value }"
          :aria-checked="modeMenu.current === option.value"
          :disabled="modeBusy"
          @click="selectMode(option.value)"
        >
          <span class="mode-option-label">{{ option.label }}</span>
        </button>
      </div>
    </div>

    <ContextMenu
      v-if="menu"
      :x="menu.x"
      :y="menu.y"
      :title="menu.upstream.name"
      @close="closeMenu"
    >
      <button @click="openEditor(menu.upstream)">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="m15 5 4 4M4 20l5-1L20 8a2.8 2.8 0 0 0-4-4L5 15z" />
        </svg>
        <span>编辑</span>
      </button>
      <button @click="ping(menu.upstream)">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="m9 15 6-6M10 7l2-2a5 5 0 0 1 7 7l-2 2M14 17l-2 2a5 5 0 0 1-7-7l2-2" />
        </svg>
        <span>测试</span>
      </button>
      <button :disabled="busy" @click="toggleEnabled(menu.upstream)">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <rect v-if="menu.upstream.enabled" x="6" y="6" width="12" height="12" rx="2" />
          <path v-else d="m8 5 11 7-11 7z" />
        </svg>
        <span>{{ menu.upstream.enabled ? '停用' : '启用' }}</span>
      </button>
      <div class="divider"></div>
      <button class="danger" @click="askDelete(menu.upstream)">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v5M14 11v5" />
        </svg>
        <span>删除</span>
      </button>
    </ContextMenu>

    <!-- 测试结果：浮在页面底部的毛玻璃提示，亮完自己隐去，不占布局。 -->
    <Transition name="float-toast">
      <FloatToast
        v-if="toast"
        :text="toast.text"
        :tone="toast.tone"
        :loading="toast.loading"
        :duration="toast.duration"
        @done="hideToast"
      />
    </Transition>

    <UpstreamForm
      v-if="editing"
      :upstream="editing.upstream"
      :suggested-port="suggestedPort"
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
