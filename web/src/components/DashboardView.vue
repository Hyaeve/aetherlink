<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'

defineProps({ status: { type: Object, default: null } })
const emit = defineEmits(['refresh-status'])

const snapshot = ref(null)
const error = ref('')
const purging = ref(false)
let timer = null

async function load() {
  try {
    snapshot.value = await api.stats(60)
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
  }
}

async function purge() {
  purging.value = true
  try {
    await api.purgeCache()
    emit('refresh-status')
  } catch (purgeError) {
    error.value = purgeError.message
  } finally {
    purging.value = false
  }
}

const redirectRate = computed(() => {
  const stats = snapshot.value
  if (!stats || !stats.totalRequests) return '0%'
  return `${Math.round((stats.redirects / stats.totalRequests) * 100)}%`
})

const cacheRate = computed(() => {
  const stats = snapshot.value
  if (!stats) return '0%'
  const total = stats.cacheHits + stats.cacheMisses
  if (!total) return '0%'
  return `${Math.round((stats.cacheHits / total) * 100)}%`
})

const kinds = computed(() => Object.entries(snapshot.value?.byKind || {}).sort((a, b) => b[1] - a[1]))

function outcomeClass(outcome) {
  if (outcome === 'redirect' || outcome === 'local') return 'tag ok'
  if (outcome === 'error' || outcome === 'unauthorized') return 'tag bad'
  if (outcome === 'proxy') return 'tag warn'
  return 'tag'
}

function shortTime(value) {
  return new Date(value).toLocaleTimeString('zh-CN', { hour12: false })
}

onMounted(() => {
  load()
  timer = setInterval(load, 5000)
})
onUnmounted(() => timer && clearInterval(timer))
</script>

<template>
  <section>
    <p v-if="error" class="error">{{ error }}</p>

    <div class="grid cols-4">
      <div class="metric">
        <div class="label">媒体请求总数</div>
        <div class="value">{{ snapshot?.totalRequests ?? 0 }}</div>
      </div>
      <div class="metric">
        <div class="label">302 重定向</div>
        <div class="value">{{ snapshot?.redirects ?? 0 }} <span class="muted" style="font-size:13px">/ {{ redirectRate }}</span></div>
      </div>
      <div class="metric">
        <div class="label">中继转发 / 本地直读</div>
        <div class="value">{{ snapshot?.proxyStreams ?? 0 }} / {{ snapshot?.localFiles ?? 0 }}</div>
      </div>
      <div class="metric">
        <div class="label">解析失败</div>
        <div class="value">{{ snapshot?.errors ?? 0 }}</div>
      </div>
    </div>

    <div class="panel" style="margin-top:16px">
      <h2>缓存与非 STRM 透传</h2>
      <div class="row">
        <span class="tag">缓存条目 {{ status?.cacheEntries ?? 0 }}</span>
        <span class="tag">TTL {{ status?.cacheTtl ?? '-' }}</span>
        <span class="tag ok">命中率 {{ cacheRate }}</span>
        <span class="tag">透传 {{ snapshot?.passthroughs ?? 0 }}</span>
        <span class="tag" v-if="status?.followUpstreamRedirects">跟随上游 302</span>
        <button :disabled="purging" @click="purge">清空解析缓存</button>
      </div>
      <div class="row" style="margin-top:12px" v-if="kinds.length">
        <span v-for="[kind, count] in kinds" :key="kind" class="tag">{{ kind }} · {{ count }}</span>
      </div>
    </div>

    <div class="panel">
      <h2>最近媒体请求</h2>
      <div class="scroll">
        <table>
          <thead>
            <tr>
              <th>时间</th>
              <th>上游</th>
              <th>结果</th>
              <th>类型</th>
              <th>目标</th>
              <th>耗时</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(event, index) in snapshot?.recentEvents || []" :key="index">
              <td class="mono">{{ shortTime(event.time) }}</td>
              <td>{{ event.upstream }}</td>
              <td>
                <span :class="outcomeClass(event.outcome)">{{ event.outcome }}</span>
                <span v-if="event.cacheHit" class="tag" style="margin-left:4px">cache</span>
              </td>
              <td>{{ event.kind || '-' }}</td>
              <td class="mono">
                {{ event.target || event.mediaPath || event.path }}
                <div v-if="event.error" class="error">{{ event.error }}</div>
              </td>
              <td class="mono">{{ Math.round((event.durationMs || 0) / 1e6) }} ms</td>
            </tr>
            <tr v-if="!(snapshot?.recentEvents || []).length">
              <td colspan="6" class="muted">还没有媒体请求。用播放器访问 AetherLink 后这里会出现记录。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </section>
</template>
