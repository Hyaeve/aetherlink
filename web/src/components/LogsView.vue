<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'

const entries = ref([])
const error = ref('')
const levelFilter = ref('all')
const autoRefresh = ref(true)
let timer = null

async function load() {
  try {
    const payload = await api.logs(300)
    entries.value = (payload.entries || []).slice().reverse()
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
  }
}

const visible = computed(() =>
  levelFilter.value === 'all' ? entries.value : entries.value.filter((entry) => entry.level === levelFilter.value)
)

function levelClass(level) {
  if (level === 'error') return 'tag bad'
  if (level === 'warn') return 'tag warn'
  if (level === 'debug') return 'tag'
  return 'tag ok'
}

function stamp(value) {
  return new Date(value).toLocaleString('zh-CN', { hour12: false })
}

onMounted(() => {
  load()
  timer = setInterval(() => autoRefresh.value && load(), 5000)
})
onUnmounted(() => timer && clearInterval(timer))
</script>

<template>
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
    <p v-if="error" class="error">{{ error }}</p>
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
