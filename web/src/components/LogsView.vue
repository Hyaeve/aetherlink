<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
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
const pageSize = 25
const eventPage = ref(1)
const logPage = ref(1)
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
    const [logPayload, statsPayload] = await Promise.all([api.logs(5000), api.stats(5000)])
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

const eventPageCount = computed(() => Math.max(1, Math.ceil(events.value.length / pageSize)))
const logPageCount = computed(() => Math.max(1, Math.ceil(visible.value.length / pageSize)))
const pagedEvents = computed(() => pageSlice(events.value, eventPage.value))
const pagedVisible = computed(() => pageSlice(visible.value, logPage.value))

watch(events, () => {
  if (eventPage.value > eventPageCount.value) eventPage.value = eventPageCount.value
})

watch(visible, () => {
  if (logPage.value > logPageCount.value) logPage.value = logPageCount.value
})

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

function cacheTTL(seconds) {
  const total = Number(seconds) || 0
  if (total <= 0) return '不缓存'
  const minutesTotal = Math.ceil(total / 60)
  if (total < 3600) return `${minutesTotal}min`
  const hours = Math.floor(minutesTotal / 60)
  const minutes = minutesTotal % 60
  return minutes === 0 ? `${hours}h` : `${hours}h ${minutes}min`
}

function userAgentText(event) {
  const clientUserAgent = event.userAgent || '空'
  const effectiveUserAgent = event.effectiveUserAgent || 'AetherLink'
  return clientUserAgent === effectiveUserAgent ? effectiveUserAgent : `${clientUserAgent} → ${effectiveUserAgent}`
}

function pageSlice(items, page) {
  const start = (page - 1) * pageSize
  return items.slice(start, start + pageSize)
}

function previousEventPage() {
  eventPage.value = Math.max(1, eventPage.value - 1)
}

function nextEventPage() {
  eventPage.value = Math.min(eventPageCount.value, eventPage.value + 1)
}

function previousLogPage() {
  logPage.value = Math.max(1, logPage.value - 1)
}

function nextLogPage() {
  logPage.value = Math.min(logPageCount.value, logPage.value + 1)
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
  <section class="logs-page">
    <p v-if="error" class="error page-error">{{ error }}</p>
    <div v-if="diagnosis" class="notice page-notice">{{ diagnosis }}</div>

    <div class="log-metrics">
      <div class="log-metric violet">
        <span class="metric-icon"><svg viewBox="0 0 24 24"><path d="M5 12h14M13 6l6 6-6 6" /></svg></span>
        <span><small>302 跳转</small><strong>{{ snapshot?.redirects ?? 0 }}</strong></span>
      </div>
      <div class="log-metric amber">
        <span class="metric-icon"><svg viewBox="0 0 24 24"><path d="M5 5h14v14H5zM9 9h6M9 13h4" /></svg></span>
        <span><small>透传上游</small><strong>{{ snapshot?.passthroughs ?? 0 }}</strong></span>
      </div>
      <div class="log-metric blue">
        <span class="metric-icon"><svg viewBox="0 0 24 24"><path d="M4 12h16M12 4v16" /><circle cx="12" cy="12" r="8" /></svg></span>
        <span><small>中继 / 本地</small><strong>{{ (snapshot?.proxyStreams ?? 0) + (snapshot?.localFiles ?? 0) }}</strong></span>
      </div>
      <div class="log-metric rose">
        <span class="metric-icon"><svg viewBox="0 0 24 24"><path d="M12 4 21 20H3zM12 9v5M12 17h.01" /></svg></span>
        <span><small>失败</small><strong>{{ snapshot?.errors ?? 0 }}</strong></span>
      </div>
    </div>

    <section class="activity-card">
      <div class="activity-head">
        <div class="activity-title">
          <span class="activity-icon violet"><svg viewBox="0 0 24 24"><path d="M4 6h16M4 12h10M4 18h16" /><circle cx="17" cy="12" r="3" /></svg></span>
          <div><h2>播放流水</h2><p>每一次媒体请求的最终处理结果</p></div>
        </div>
        <div class="activity-actions">
          <select v-model="outcomeFilter" aria-label="筛选播放结果">
            <option value="all">全部结果</option>
            <option value="redirect">302 跳转</option>
            <option value="passthrough">透传上游</option>
            <option value="proxy">AetherLink 中继</option>
            <option value="local">本地直读</option>
            <option value="error">失败</option>
          </select>
          <button class="icon-button" title="立即刷新" aria-label="立即刷新" @click="load">
            <svg viewBox="0 0 24 24"><path d="M20 11a8 8 0 0 0-14.8-4L4 9" /><path d="M4 5v4h4M4 13a8 8 0 0 0 14.8 4L20 15" /><path d="M20 19v-4h-4" /></svg>
          </button>
        </div>
      </div>
      <div class="table-wrap">
        <table>
          <thead>
            <tr><th>时间</th><th>上游</th><th>请求路径</th><th>结果</th><th>目标</th><th>UA</th><th>缓存有效期</th><th>耗时</th></tr>
          </thead>
          <tbody>
            <tr v-for="(event, index) in pagedEvents" :key="index">
              <td>{{ clock(event.time) }}</td>
              <td>{{ event.upstream }}</td>
              <td class="mono">{{ shorten(event.path, 48) }}</td>
              <td><span :class="outcomeClass(event.outcome)">{{ outcomeLabel(event.outcome) }}</span></td>
              <td class="target-cell">
                <span
                  class="target-box mono"
                  :title="event.error || event.target || event.mediaPath || ''"
                >{{ shorten(event.error || event.target || event.mediaPath) }}</span>
              </td>
              <td class="target-cell">
                <span
                  class="target-box mono"
                  :title="userAgentText(event)"
                >{{ userAgentText(event) }}</span>
              </td>
              <td>{{ cacheTTL(event.cacheTtlSeconds) }}</td>
              <td>{{ millis(event.durationMs) }}</td>
            </tr>
          </tbody>
        </table>
        <div v-if="!events.length" class="empty-inline">
          <svg viewBox="0 0 24 24"><path d="M5 5h14v14H5zM8 9h8M8 13h5" /></svg>
          <span>还没有播放请求</span>
        </div>
        <div v-if="eventPageCount > 1" class="pager" aria-label="播放流水分页">
          <button class="pager-button" :disabled="eventPage <= 1" @click="previousEventPage">上一页</button>
          <span>第 {{ eventPage }} / {{ eventPageCount }} 页 · 共 {{ events.length }} 条</span>
          <button class="pager-button" :disabled="eventPage >= eventPageCount" @click="nextEventPage">下一页</button>
        </div>
      </div>
    </section>

    <section class="activity-card">
      <div class="activity-head">
        <div class="activity-title">
          <span class="activity-icon blue"><svg viewBox="0 0 24 24"><path d="M5 5h14v14H5zM8 9h8M8 12h8M8 15h5" /></svg></span>
          <div><h2>服务日志</h2><p>查看 AetherLink 的运行状态与诊断信息</p></div>
        </div>
        <div class="activity-actions">
          <select v-model="levelFilter" aria-label="筛选日志级别">
            <option value="all">全部级别</option>
            <option value="debug">debug</option>
            <option value="info">info</option>
            <option value="warn">warn</option>
            <option value="error">error</option>
          </select>
          <label class="refresh-toggle">
            <input type="checkbox" v-model="autoRefresh" />
            <span class="toggle-control"></span>
            <span>自动刷新</span>
          </label>
          <button class="icon-button" title="立即刷新" aria-label="立即刷新" @click="load">
            <svg viewBox="0 0 24 24"><path d="M20 11a8 8 0 0 0-14.8-4L4 9" /><path d="M4 5v4h4M4 13a8 8 0 0 0 14.8 4L20 15" /><path d="M20 19v-4h-4" /></svg>
          </button>
        </div>
      </div>
      <div class="log-stream">
        <div v-for="(entry, index) in pagedVisible" :key="index" class="log-line">
          <span class="log-time">{{ stamp(entry.time) }}</span>
          <span :class="levelClass(entry.level)">{{ entry.level }}</span>
          <span class="log-message" :title="entry.message">{{ entry.message }}</span>
        </div>
        <div v-if="!visible.length" class="empty-inline">
          <svg viewBox="0 0 24 24"><path d="M5 5h14v14H5zM8 9h8M8 13h5" /></svg>
          <span>暂无日志</span>
        </div>
        <div v-if="logPageCount > 1" class="pager" aria-label="服务日志分页">
          <button class="pager-button" :disabled="logPage <= 1" @click="previousLogPage">上一页</button>
          <span>第 {{ logPage }} / {{ logPageCount }} 页 · 共 {{ visible.length }} 条</span>
          <button class="pager-button" :disabled="logPage >= logPageCount" @click="nextLogPage">下一页</button>
        </div>
      </div>
    </section>
  </section>
</template>
