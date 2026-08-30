<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'

// 这一页有两块：上半是「播放流水」，下半是「运行日志」。
// 播放流水来自 /stats，逐条记录每个媒体请求最终是 302、透传还是中继；
// 排查「为什么没有 302」时它比文本日志更直接，因为原因就在同一行里。
const entries = ref([])
const snapshot = ref(null)
const error = ref('')
const levelFilter = ref('all')
const outcomeFilter = ref('all')
const autoRefresh = ref(true)
let timer = null

const OUTCOME_LABELS = {
  redirect: '302 跳转',
  passthrough: '透传上游',
  proxy: 'AetherLink 中继',
  local: '本地直读',
  error: '失败',
  unauthorized: '未授权'
}

async function load() {
  try {
    const [logPayload, statsPayload] = await Promise.all([api.logs(300), api.stats(150)])
    entries.value = (logPayload.entries || []).slice().reverse()
    snapshot.value = statsPayload
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
  }
}

const events = computed(() => {
  const all = snapshot.value?.recentEvents || []
  return outcomeFilter.value === 'all' ? all : all.filter((event) => event.outcome === outcomeFilter.value)
})

const visible = computed(() =>
  levelFilter.value === 'all' ? entries.value : entries.value.filter((entry) => entry.level === levelFilter.value)
)

// 一眼能看出问题的诊断结论：有播放请求但一次都没跳转，就直接说清最可能的原因。
const diagnosis = computed(() => {
  const stats = snapshot.value
  if (!stats || !stats.totalRequests) return ''
  if (stats.redirects > 0) return ''
  if (stats.passthroughs > 0) {
    const reason = (stats.recentEvents || []).find((event) => event.outcome === 'passthrough' && event.error)
    if (reason) return `拦截到了播放请求但全部透传，最近一条的原因：${reason.error}`
    return '拦截到了播放请求，但上游报告的媒体不是 strm 指针，因此没有可跳转的地址。'
  }
  return ''
})

function outcomeLabel(outcome) {
  return OUTCOME_LABELS[outcome] || outcome
}

function outcomeClass(outcome) {
  if (outcome === 'redirect' || outcome === 'local') return 'tag ok'
  if (outcome === 'passthrough' || outcome === 'proxy') return 'tag warn'
  return 'tag bad'
}

function levelClass(level) {
  if (level === 'error') return 'tag bad'
  if (level === 'warn') return 'tag warn'
  if (level === 'debug') return 'tag'
  return 'tag ok'
}

function stamp(value) {
  return new Date(value).toLocaleString('zh-CN', { hour12: false })
}

function clock(value) {
  return new Date(value).toLocaleTimeString('zh-CN', { hour12: false })
}

// 后端的 durationMs 是 Go 的 time.Duration（纳秒），显示前换成毫秒。
function millis(duration) {
  return `${Math.round((Number(duration) || 0) / 1e6)} ms`
}

function shorten(value, max = 64) {
  if (!value) return '—'
  return value.length > max ? `${value.slice(0, max)}…` : value
}

onMounted(() => {
  load()
  timer = setInterval(() => autoRefresh.value && load(), 5000)
})
onUnmounted(() => timer && clearInterval(timer))
</script>

<template>
  <section class="panel" style="margin-bottom:18px">
    <h2>播放流水</h2>
    <p v-if="error" class="error">{{ error }}</p>
    <div v-if="diagnosis" class="notice">{{ diagnosis }}</div>

    <div class="grid cols-4" style="margin-bottom:14px">
      <div class="metric">
        <div class="label">302 跳转</div>
        <div class="value">{{ snapshot?.redirects ?? 0 }}</div>
      </div>
      <div class="metric">
        <div class="label">透传上游</div>
        <div class="value">{{ snapshot?.passthroughs ?? 0 }}</div>
      </div>
      <div class="metric">
        <div class="label">中继 / 本地</div>
        <div class="value">{{ (snapshot?.proxyStreams ?? 0) + (snapshot?.localFiles ?? 0) }}</div>
      </div>
      <div class="metric">
        <div class="label">失败</div>
        <div class="value">{{ snapshot?.errors ?? 0 }}</div>
      </div>
    </div>

    <div class="row" style="margin-bottom:12px">
      <select v-model="outcomeFilter" style="max-width:180px">
        <option value="all">全部结果</option>
        <option value="redirect">302 跳转</option>
        <option value="passthrough">透传上游</option>
        <option value="proxy">AetherLink 中继</option>
        <option value="local">本地直读</option>
        <option value="error">失败</option>
      </select>
      <button @click="load">立即刷新</button>
    </div>

    <div class="scroll">
      <table>
        <thead>
          <tr>
            <th>时间</th>
            <th>上游</th>
            <th>请求路径</th>
            <th>结果</th>
            <th>目标</th>
            <th>耗时</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(event, index) in events" :key="index">
            <td>{{ clock(event.time) }}</td>
            <td>{{ event.upstream }}</td>
            <td class="mono">{{ shorten(event.path, 48) }}</td>
            <td>
              <span :class="outcomeClass(event.outcome)">{{ outcomeLabel(event.outcome) }}</span>
            </td>
            <td class="mono">{{ shorten(event.error || event.target || event.mediaPath) }}</td>
            <td>{{ millis(event.durationMs) }}</td>
          </tr>
        </tbody>
      </table>
      <p v-if="!events.length" class="muted">还没有播放请求。播放一次媒体，这里就会出现对应的处理结果。</p>
    </div>
  </section>

  <section class="panel">
    <h2>运行日志</h2>
    <div class="row" style="margin-bottom:12px">
      <select v-model="levelFilter" style="max-width:160px">
        <option value="all">全部级别</option>
        <option value="debug">debug</option>
        <option value="info">info</option>
        <option value="warn">warn</option>
        <option value="error">error</option>
      </select>
      <label class="row" style="gap:6px">
        <input type="checkbox" v-model="autoRefresh" style="width:auto" />
        <span class="muted">自动刷新</span>
      </label>
      <button @click="load">立即刷新</button>
    </div>
    <div class="scroll mono">
      <div v-for="(entry, index) in visible" :key="index" class="log-line">
        <span class="muted">{{ stamp(entry.time) }}</span>
        <span :class="levelClass(entry.level)">{{ entry.level }}</span>
        <span>{{ entry.message }}</span>
      </div>
      <p v-if="!visible.length" class="muted">暂无日志。</p>
    </div>
  </section>
</template>