<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { api, visibleMessage } from '../api'

// 这一页有两块：上半是「播放流水」，下半是「运行日志」。
// 播放流水来自 /stats，逐条记录每个媒体请求最终是 302、透传还是中继；
// 排查「为什么没有 302」时它比文本日志更直接，因为原因就在同一行里。
const entries = ref([])
const emit = defineEmits(['stats'])
const snapshot = ref(null)
const error = ref('')
const levelFilter = ref('all')
const outcomeFilter = ref('all')
const autoRefresh = ref(true)
// 虚拟滚动（2026-09-22 起不再分页）：两块列表各自只渲染视口附近的行，用上下两个
// 占位元素把滚动条撑到真实长度，滚到底就是最后一条。播放流水表的行高一致，一个实测
// 值就够；服务日志允许折行（用户要求「允许日志多行展示」），行高不再整齐，改用逐行
// 实测高度 + 前缀和，见下面的 LOG_ROW_ESTIMATE / logOffsets / measureLogRows。
const EVENT_ROW_FALLBACK = 41
const OVERSCAN = 8
// 日志行高的兜底值与缓存上限：一条都没量到时按 ESTIMATE 估，量到多少就是多少。
// 高度按「级别 + 正文」缓存 —— 列表每 5 秒整份刷一次，下标会变而内容不变，按下标存
// 等于每轮轮询都退回估算值、滚动条跟着跳。上限只是防一份超长运行的日志把内存撑起来。
const LOG_ROW_ESTIMATE = 43
const LOG_HEIGHTS_LIMIT = 20000
const logHeights = new Map()
const logHeightsVersion = ref(0)
const eventScroller = ref(null)
const logScroller = ref(null)
const eventScrollTop = ref(0)
const logScrollTop = ref(0)
const eventViewport = ref(0)
const logViewport = ref(0)
const eventRowHeight = ref(EVENT_ROW_FALLBACK)
// 折行位置只跟宽度有关，记下上次量到的宽度：没变就不必把量好的高度全丢掉重来。
let logMeasuredWidth = 0
const copiedTarget = ref('')
const copiedUserAgent = ref('')
const hoverTooltip = ref(null)
const tooltipElement = ref(null)
let timer = null
let copyTimer = null
let tooltipTimer = null
let observer = null

const OUTCOME_LABELS = {
  redirect: '302 跳转',
  passthrough: '透传上游',
  proxy: '中继',
  transcode: '音频兼容中继',
  local: '本地直读',
  error: '失败',
  unauthorized: '未授权'
}

async function load() {
  try {
    const [logPayload, statsPayload] = await Promise.all([api.logs(5000), api.stats(5000)])
    entries.value = (logPayload.entries || []).slice().reverse()
    snapshot.value = statsPayload
    emit('stats', {
      requests: statsPayload.totalRequests || 0,
      redirects: statsPayload.redirects || 0,
      relay: (statsPayload.proxyStreams || 0) + (statsPayload.transcodes || 0) + (statsPayload.passthroughs || 0),
      errors: statsPayload.errors || 0,
      logLines: entries.value.length
    })
    error.value = ''
    // 数据换了就重新量一次视口与行高：第一次渲染之前量不到行，实测值要靠这里落到
    // 变量上；窗口/卡片尺寸也会在两次刷新之间变。
    await nextTick()
    syncVirtualList()
  } catch (loadError) {
    // 这个页面每 5 秒轮询一次，容器一重启它往往比其他请求更早撞上 401。会话失效
    // 已经由 api 层处理成「清令牌 + 回登录页」，这里必须显示空串，否则那句
    // 「会话无效或已过期，请重新登录」会先挂成一条横栏，等主状态轮询（15 秒）才消失。
    error.value = visibleMessage(loadError)
  }
}

const events = computed(() => {
  const all = snapshot.value?.recentEvents || []
  return outcomeFilter.value === 'all' ? all : all.filter((event) => event.outcome === outcomeFilter.value)
})

const visible = computed(() =>
  levelFilter.value === 'all' ? entries.value : entries.value.filter((entry) => entry.level === levelFilter.value)
)

const eventWindow = computed(() =>
  windowRange(events.value.length, eventScrollTop.value, eventViewport.value, eventRowHeight.value)
)
const visibleEvents = computed(() => events.value.slice(eventWindow.value.start, eventWindow.value.end))
// logOffsets 是每行「上边界」的高度前缀和（长度 = 行数 + 1），变高列表的全部几何都从
// 它出来：二分找视口首行、上下占位取差值。它只依赖列表与实测高度、不依赖滚动位置，
// 所以滚动本身不会触发重算。后端日志是 5000 条环形缓冲，重算成本可以忽略。
const logOffsets = computed(() => {
  // 读一下版本号建立依赖：量到新高度时它 ++，这份前缀和才会重算。
  void logHeightsVersion.value
  const list = visible.value
  const offsets = new Array(list.length + 1)
  offsets[0] = 0
  for (let index = 0; index < list.length; index += 1) {
    offsets[index + 1] = offsets[index] + logRowHeightOf(list[index])
  }
  return offsets
})
const logWindow = computed(() => logWindowRange(logOffsets.value, logScrollTop.value, logViewport.value))
const visibleLogs = computed(() => visible.value.slice(logWindow.value.start, logWindow.value.end))

// 换筛选条件等于换了一份列表，滚动位置得回到顶部，否则会停在一片空白上。
function resetScroll(scroller, scrollTop) {
  scrollTop.value = 0
  nextTick(() => {
    if (scroller.value) scroller.value.scrollTop = 0
  })
}

watch(outcomeFilter, () => resetScroll(eventScroller, eventScrollTop))
watch(levelFilter, () => resetScroll(logScroller, logScrollTop))
// 渲染完补量一次日志行高：滚动、筛选与 5 秒轮询都会把新行带进视口，它们的真实高度
// 只有渲染完才量得到。flush: 'post' 保证量的时候 DOM 已经更新；量到的变化只会让前缀和
// 重算一轮（第二次量到的值与缓存相同就停），不会来回转。
watch(logWindow, () => measureLogRows(), { flush: 'post' })

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
  if (outcome === 'passthrough' || outcome === 'proxy' || outcome === 'transcode') return 'tag warn'
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
  const date = new Date(value)
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${month}/${day} ${date.toLocaleTimeString('zh-CN', { hour12: false })}`
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

function cacheSourceLabel(event) {
  if (event.cacheSource === 'restored') return '恢复命中'
  if (event.cacheSource === 'hit' || event.cacheHit) return '缓存命中'
  return '首次获取'
}

function cacheSourceClass(event) {
  if (event.cacheSource === 'restored') return 'tag ok'
  if (event.cacheSource === 'hit' || event.cacheHit) return 'tag'
  return 'tag warn'
}

function userAgentText(event) {
  const clientUserAgent = event.userAgent || '空'
  const effectiveUserAgent = event.effectiveUserAgent || 'AetherLink'
  return clientUserAgent === effectiveUserAgent ? effectiveUserAgent : `${clientUserAgent} → ${effectiveUserAgent}`
}

function targetText(event) {
  return event.target || event.mediaPath || event.error || ''
}

function copyableTarget(event) {
  return event.target || ''
}

// 「UA」栏能复制的是客户端发来的原始 UA，也就是表格里箭头左边那半：它是客户端的
// 身份，也是唯一能直接粘进「屏蔽 UA」名单或上游 UA 匹配里的值。箭头右边是我们按
// 配置改写后发给上游的 UA（由「转发客户端 UA」与回退 UA 共同决定），派生自左边，
// 复制它没有意义。客户端压根没带 UA 时不可点——与「目标」栏没有可跳转地址时一致。
function copyableUserAgent(event) {
  return event.userAgent || ''
}

// 这一栏同时显示两个 UA，所以悬停提示要写明点下去复制的是哪一个，否则用户得去记
// 「复制的是左边那个」这种没写在界面上的规矩。
function copyHint(event) {
  if (!copyableUserAgent(event)) return ''
  return event.effectiveUserAgent && event.effectiveUserAgent !== event.userAgent
    ? '点击复制左侧的客户端 UA'
    : '点击复制 UA'
}

function copiedLabel(copied, value, fallback) {
  return copied && copied === value ? '已复制' : fallback
}

// 复制成功才亮「已复制」，失败不冒充成功。两条复制路径共用这一段剪贴板逻辑。
async function writeClipboard(text) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
    } else {
      legacyCopy(text)
    }
    return true
  } catch {
    try {
      legacyCopy(text)
    } catch {
      return false
    }
  }
}

// 高亮只留最近复制的那一格，1.6 秒后自动褪掉。
function markCopied(field, value) {
  copiedTarget.value = field === 'target' ? value : ''
  copiedUserAgent.value = field === 'userAgent' ? value : ''
  if (copyTimer) clearTimeout(copyTimer)
  copyTimer = setTimeout(() => {
    copiedTarget.value = ''
    copiedUserAgent.value = ''
  }, 1600)
}

async function copyTarget(event) {
  const target = copyableTarget(event)
  if (!target || !(await writeClipboard(target))) return
  markCopied('target', target)
}

async function copyUserAgent(event) {
  const userAgent = copyableUserAgent(event)
  if (!userAgent || !(await writeClipboard(userAgent))) return
  markCopied('userAgent', userAgent)
}

function legacyCopy(target) {
    const textarea = document.createElement('textarea')
    textarea.value = target
    textarea.style.position = 'fixed'
    textarea.style.opacity = '0'
    document.body.appendChild(textarea)
    textarea.select()
    const copied = document.execCommand('copy')
    textarea.remove()
    if (!copied) throw new Error('无法复制到剪贴板')
}

// windowRange 是虚拟滚动的全部数学：给「总条数 + 当前滚动位置 + 视口高度 + 行高」，
// 算出该渲染哪一段（start..end）与上下各留多高的占位。前后各多渲染 OVERSCAN 行，
// 滚动时边缘才不会先看到空白再补上。
function windowRange(total, scrollTop, viewport, rowHeight) {
  if (total <= 0 || rowHeight <= 0) return { start: 0, end: 0, top: 0, bottom: 0 }
  const first = Math.floor(Math.max(0, scrollTop) / rowHeight)
  const start = Math.max(0, first - OVERSCAN)
  const wanted = Math.ceil(Math.max(viewport, rowHeight) / rowHeight) + OVERSCAN * 2
  const end = Math.min(total, start + wanted)
  return { start, end, top: start * rowHeight, bottom: (total - end) * rowHeight }
}

// logWindowRange 是「行高不一」那一档的算法（服务日志用），几何全部来自前缀和
// offsets（长度 = 行数 + 1，offsets[i] 是第 i 行的上边界）。先二分出视口顶部落在哪一行，
// 再往下累加到铺满视口，前后各放 OVERSCAN 行。上下占位直接取前缀和差值 —— 行高不整齐时
// 「下标 × 行高」算出来的位置是错的，滚动条长度会与实际内容对不上。
function logWindowRange(offsets, scrollTop, viewport) {
  const total = offsets.length - 1
  if (total <= 0) return { start: 0, end: 0, top: 0, bottom: 0 }
  const first = firstVisibleRow(offsets, Math.max(0, scrollTop))
  const bottomEdge = Math.max(0, scrollTop) + Math.max(viewport, LOG_ROW_ESTIMATE)
  let end = first
  while (end < total && offsets[end] < bottomEdge) end += 1
  const start = Math.max(0, first - OVERSCAN)
  end = Math.min(total, Math.max(end + OVERSCAN, first + 1))
  return { start, end, top: offsets[start], bottom: offsets[total] - offsets[end] }
}

// firstVisibleRow 二分找出「最后一个上边界 <= scrollTop 的行」。offsets[0] 恒为 0，
// 所以结果一定落在 [0, total]，滚到底时返回的就是最后一行。
function firstVisibleRow(offsets, scrollTop) {
  let low = 0
  let high = offsets.length - 1
  while (low < high) {
    const middle = Math.ceil((low + high) / 2)
    if (offsets[middle] <= scrollTop) low = middle
    else high = middle - 1
  }
  return low
}

// 日志行高按内容缓存，键取「级别 + 正文」而不是下标：列表每 5 秒整份换新，下标会变。
function logHeightKey(entry) {
  return `${entry.level}\u0000${entry.message}`
}

function logRowHeightOf(entry) {
  return logHeights.get(logHeightKey(entry)) || LOG_ROW_ESTIMATE
}

function onEventScroll(event) {
  eventScrollTop.value = event.target.scrollTop
}

function onLogScroll(event) {
  logScrollTop.value = event.target.scrollTop
}

// invalidateLogHeights 把整份实测高度作废。只有宽度变了才需要：折行位置只由宽度决定，
// 高度也是按内容缓存的，所以宽度一变旧值就全错；清掉之后下一帧会重新量。
function invalidateLogHeights() {
  if (logHeights.size === 0) return
  logHeights.clear()
  logHeightsVersion.value += 1
}

// refreshViewport 量两块滚动区的高度。窗口缩放、侧栏展开、卡片高度跟着视口走，
// 都靠它跟上 —— 只量一次会算错该渲染多少行。
function refreshViewport() {
  if (eventScroller.value) eventViewport.value = eventScroller.value.clientHeight
  if (!logScroller.value) return
  logViewport.value = logScroller.value.clientHeight
  const width = logScroller.value.clientWidth
  if (width !== logMeasuredWidth) {
    logMeasuredWidth = width
    invalidateLogHeights()
  }
}

// measureRowHeight 量「行高一致」那一档的一行真实高度，量到就记住。量不到（列表空、
// 还没渲染）就保持上一次的值，别退回兜底值 —— 那会让滚动位置整体偏一截。
function measureRowHeight(scroller, selector, target) {
  const row = scroller.value?.querySelector(selector)
  if (!row) return
  const height = row.getBoundingClientRect().height
  if (height > 1) target.value = height
}

// measureLogRows 把当前渲染出来的日志行逐个量一遍，按内容记进 logHeights。只有高度真的
// 变了（差 0.5px 以上）才计数，一帧里的多处改动在末尾合并成**一次**前缀和重算 ——
// 否则「量 → 失效 → 重算 → 再量」会自己转圈。
function measureLogRows() {
  const scroller = logScroller.value
  if (!scroller) return
  let changed = false
  scroller.querySelectorAll('.log-line').forEach((row) => {
    const entry = visible.value[Number(row.dataset.index)]
    if (!entry) return
    const height = row.getBoundingClientRect().height
    if (height <= 1) return
    const key = logHeightKey(entry)
    const stored = logHeights.get(key)
    if (stored !== undefined && Math.abs(stored - height) < 0.5) return
    if (logHeights.size >= LOG_HEIGHTS_LIMIT) logHeights.clear()
    logHeights.set(key, height)
    changed = true
  })
  if (changed) logHeightsVersion.value += 1
}

function syncVirtualList() {
  refreshViewport()
  measureRowHeight(eventScroller, 'tbody tr:not(.virtual-spacer)', eventRowHeight)
  measureLogRows()
}

function isTruncated(target) {
  const content = target.querySelector?.('.target-box') || target
  if (content.scrollWidth > content.clientWidth || content.scrollHeight > content.clientHeight) return true
  const range = document.createRange()
  range.selectNodeContents(content)
  const style = getComputedStyle(content)
  const availableWidth = content.getBoundingClientRect().width
    - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight)
    - parseFloat(style.borderLeftWidth) - parseFloat(style.borderRightWidth)
  return range.getBoundingClientRect().width > availableWidth
}

function showTooltip(value, event, hint = '') {
  if (tooltipTimer) clearTimeout(tooltipTimer)
  hoverTooltip.value = null
  const target = event.currentTarget
  if (!value) {
    hoverTooltip.value = null
    return
  }
  tooltipTimer = setTimeout(async () => {
    if (!target.isConnected || !isTruncated(target)) return
    const rect = target.getBoundingClientRect()
    const maxWidth = Math.min(760, window.innerWidth - 32)
    const left = Math.max(16, rect.left)
    hoverTooltip.value = {
      text: value,
      hint,
      style: {
        left: `${left}px`,
        top: `${Math.min(window.innerHeight - 24, rect.bottom + 8)}px`,
        maxWidth: `${maxWidth}px`
      }
    }
    const currentTooltip = hoverTooltip.value
    await nextTick()
    if (hoverTooltip.value !== currentTooltip || !tooltipElement.value) return
    const bounds = tooltipElement.value.getBoundingClientRect()
    currentTooltip.style.left = `${Math.max(16, Math.min(left, window.innerWidth - bounds.width - 16))}px`
    currentTooltip.style.top = `${Math.max(16, Math.min(rect.bottom + 8, window.innerHeight - bounds.height - 16))}px`
  }, 600)
}

function hideTooltip() {
  if (tooltipTimer) {
    clearTimeout(tooltipTimer)
    tooltipTimer = null
  }
  hoverTooltip.value = null
}

onMounted(async () => {
  await load()
  window.addEventListener('resize', syncVirtualList)
  // 卡片高度跟着视口伸缩，视口尺寸不能只量一次。ResizeObserver 比 window.resize 更
  // 贴切：侧栏展开、筛选后行数变化都会改这块区域的高度。
  if (typeof ResizeObserver === 'function') {
    observer = new ResizeObserver(syncVirtualList)
    if (eventScroller.value) observer.observe(eventScroller.value)
    if (logScroller.value) observer.observe(logScroller.value)
  }
  timer = setInterval(() => autoRefresh.value && load(), 5000)
})
onUnmounted(() => {
  window.removeEventListener('resize', syncVirtualList)
  if (observer) observer.disconnect()
  if (timer) clearInterval(timer)
  if (copyTimer) clearTimeout(copyTimer)
  if (tooltipTimer) clearTimeout(tooltipTimer)
})
</script>

<template>
  <section class="logs-page">
    <div v-if="hoverTooltip" ref="tooltipElement" class="log-floating-tooltip" :style="hoverTooltip.style">{{ hoverTooltip.text }}<span v-if="hoverTooltip.hint" class="log-floating-tooltip-hint">{{ hoverTooltip.hint }}</span></div>
    <p v-if="error" class="error page-error">{{ error }}</p>
    <div v-if="diagnosis" class="notice page-notice">{{ diagnosis }}</div>

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
            <option value="proxy">中继</option>
            <option value="transcode">音频兼容中继</option>
            <option value="local">本地直读</option>
            <option value="error">失败</option>
          </select>
          <button class="icon-button" title="立即刷新" aria-label="立即刷新" @click="load">
            <svg viewBox="0 0 24 24"><path d="M20 11a8 8 0 0 0-14.8-4L4 9" /><path d="M4 5v4h4M4 13a8 8 0 0 0 14.8 4L20 15" /><path d="M20 19v-4h-4" /></svg>
          </button>
        </div>
      </div>
      <div ref="eventScroller" class="table-wrap" @scroll="onEventScroll">
        <table class="playback-table">
          <colgroup>
            <col class="playback-time-column" />
            <col class="playback-upstream-column" />
            <col class="playback-ua-column" />
            <col class="playback-outcome-column" />
            <col class="playback-target-column" />
            <col class="playback-client-column" />
            <col class="playback-cache-column" />
            <col class="playback-ttl-column" />
            <col class="playback-duration-column" />
          </colgroup>
          <thead>
            <tr><th>时间</th><th>上游</th><th>UA</th><th>结果</th><th>目标</th><th>客户端 IP</th><th>缓存状态</th><th>缓存有效期</th><th>耗时</th></tr>
          </thead>
          <tbody>
            <!-- 上下两个占位行把滚动条撑到真实长度，中间只渲染视口附近那一段。 -->
            <tr v-if="eventWindow.top" class="virtual-spacer" :style="{ height: `${eventWindow.top}px` }" aria-hidden="true"><td :colspan="9"></td></tr>
            <tr v-for="(event, index) in visibleEvents" :key="eventWindow.start + index">
              <td>{{ clock(event.time) }}</td>
              <td
                class="target-cell"
                @mouseenter="showTooltip(event.upstream, $event)"
                @mouseleave="hideTooltip"
                @focusin="showTooltip(event.upstream, $event)"
                @focusout="hideTooltip"
              >
                <span class="target-box" tabindex="0">{{ event.upstream || '—' }}</span>
              </td>
              <td
                class="target-cell"
                @mouseenter="showTooltip(copiedLabel(copiedUserAgent, copyableUserAgent(event), userAgentText(event)), $event, copyHint(event))"
                @mouseleave="hideTooltip"
                @focusin="showTooltip(copiedLabel(copiedUserAgent, copyableUserAgent(event), userAgentText(event)), $event, copyHint(event))"
                @focusout="hideTooltip"
              >
                <button
                  type="button"
                  class="target-box mono"
                  :class="{ 'is-copyable': copyableUserAgent(event) }"
                  :disabled="!copyableUserAgent(event)"
                  @click="copyUserAgent(event)"
                >{{ userAgentText(event) }}</button>
              </td>
              <td><span :class="outcomeClass(event.outcome)">{{ outcomeLabel(event.outcome) }}</span></td>
              <td
                class="target-cell"
                @mouseenter="showTooltip(copiedLabel(copiedTarget, copyableTarget(event), targetText(event)), $event)"
                @mouseleave="hideTooltip"
                @focusin="showTooltip(copiedLabel(copiedTarget, copyableTarget(event), targetText(event)), $event)"
                @focusout="hideTooltip"
              >
                <button
                  type="button"
                  class="target-box mono"
                  :class="{ 'is-copyable': copyableTarget(event) }"
                  :disabled="!copyableTarget(event)"
                  @click="copyTarget(event)"
                >{{ targetText(event) || '—' }}</button>
              </td>
              <td class="target-cell" @mouseenter="showTooltip(event.client || '未知', $event)" @mouseleave="hideTooltip" @focusin="showTooltip(event.client || '未知', $event)" @focusout="hideTooltip">
                <span class="target-box mono" tabindex="0">{{ event.client || '未知' }}</span>
              </td>
              <td><span :class="cacheSourceClass(event)">{{ cacheSourceLabel(event) }}</span></td>
              <td>{{ cacheTTL(event.cacheTtlSeconds) }}</td>
              <td>{{ millis(event.durationMs) }}</td>
            </tr>
            <tr v-if="eventWindow.bottom" class="virtual-spacer" :style="{ height: `${eventWindow.bottom}px` }" aria-hidden="true"><td :colspan="9"></td></tr>
          </tbody>
        </table>
        <div v-if="!events.length" class="empty-inline">
          <svg viewBox="0 0 24 24"><path d="M5 5h14v14H5zM8 9h8M8 13h5" /></svg>
          <span>还没有播放请求</span>
        </div>
      </div>
      <div class="list-count">共 {{ events.length }} 条 · 滚动浏览</div>
    </section>

    <section class="activity-card log-card">
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
      <div ref="logScroller" class="log-stream" @scroll="onLogScroll">
        <div v-if="logWindow.top" class="virtual-spacer" :style="{ height: `${logWindow.top}px` }" aria-hidden="true"></div>
        <div v-for="(entry, index) in visibleLogs" :key="logWindow.start + index" class="log-line" :data-index="logWindow.start + index">
          <span class="log-time">{{ stamp(entry.time) }}</span>
          <span :class="levelClass(entry.level)">{{ entry.level }}</span>
          <span
            class="log-message"
            @mouseenter="showTooltip(entry.message, $event)"
            @mouseleave="hideTooltip"
            @focusin="showTooltip(entry.message, $event)"
            @focusout="hideTooltip"
          >{{ entry.message }}</span>
        </div>
        <div v-if="logWindow.bottom" class="virtual-spacer" :style="{ height: `${logWindow.bottom}px` }" aria-hidden="true"></div>
        <div v-if="!visible.length" class="empty-inline">
          <svg viewBox="0 0 24 24"><path d="M5 5h14v14H5zM8 9h8M8 13h5" /></svg>
          <span>暂无日志</span>
        </div>
      </div>
      <div class="list-count">共 {{ visible.length }} 条 · 滚动浏览</div>
    </section>
  </section>
</template>
