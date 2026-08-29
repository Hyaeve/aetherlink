<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'
import { gradientFor } from '../palette'
import ContextMenu from './ContextMenu.vue'
import UpstreamForm from './UpstreamForm.vue'

const emit = defineEmits(['changed'])

const upstreams = ref([])
const error = ref('')
const notice = ref('')

// editing: null 不打开；{ upstream: null } 新增；{ upstream } 编辑。
const editing = ref(null)
const menu = ref(null)
const pendingDelete = ref(null)
const busy = ref(false)

const TYPE_LABELS = { audiobookshelf: 'Audiobookshelf', emby: 'Emby' }

async function load() {
  try {
    const payload = await api.upstreams()
    upstreams.value = payload.upstreams || []
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
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

function typeLabel(type) {
  return TYPE_LABELS[type] || type
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
  <section>
    <div class="section-head">
      <h2>反代上游</h2>
      <span class="muted" style="font-size:12.5px">右键卡片可编辑、试连或删除。保存后立即生效，无需重启容器。</span>
    </div>

    <p v-if="error" class="error" style="margin-bottom:12px">{{ error }}</p>
    <div v-if="notice" class="notice">{{ notice }}</div>

    <div class="card-grid">
      <div
        v-for="upstream in upstreams"
        :key="upstream.name"
        class="card"
        :class="{ dimmed: !upstream.enabled }"
        :style="{ background: gradientFor(upstream.name) }"
        @click="openEditor(upstream)"
        @contextmenu.prevent="openMenu($event, upstream)"
      >
        <svg class="card-icon" viewBox="0 0 24 24" aria-hidden="true">
          <path d="M4 7a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z" />
          <path d="M12 10v5" />
          <path d="M9.5 12.5 12 15l2.5-2.5" />
        </svg>
        <span class="card-hint">右键更多</span>

        <div class="card-body">
          <div class="card-title">{{ upstream.name }}</div>
          <div class="card-meta">{{ hostLabel(upstream.baseUrl) }}</div>
          <div class="card-chips">
            <span class="chip">{{ typeLabel(upstream.type) }}</span>
            <span class="chip" v-if="upstream.prefix && upstream.prefix !== '/'">{{ upstream.prefix }}</span>
            <span class="chip bad" v-if="!upstream.enabled">已停用</span>
            <span class="chip warn" v-else-if="!upstream.hasApiKey">缺密钥</span>
            <span class="chip warn" v-else-if="!upstream.active">未挂载</span>
          </div>
        </div>
      </div>

      <button class="card-add" @click="openEditor(null)">
        <span class="plus">+</span>
        <span>添加上游</span>
      </button>
    </div>

    <p v-if="!upstreams.length" class="muted" style="margin-top:16px;font-size:13px">
      还没有上游。点「添加上游」，填入 Audiobookshelf 的地址和 API 密钥
      （设置 → 用户 → 该用户的 API Token），AetherLink 就能读到它的书库并对 strm 做 302 播放。
    </p>

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
      @close="editing = null"
      @saved="onSaved"
    />

    <div v-if="pendingDelete" class="modal-backdrop" @click.self="pendingDelete = null">
      <div class="modal" style="width:min(420px,100%)">
        <div class="modal-head"><h2>删除上游</h2></div>
        <div class="modal-body">
          <p style="margin:0;font-size:13.5px;line-height:1.6">
            确认删除 <strong>{{ pendingDelete.name }}</strong>？
            它的地址与 API 密钥会一并从配置里移除，指向该入口的播放器会立刻失效。
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
