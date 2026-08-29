<script setup>
import { onMounted, ref, watch } from 'vue'
import { api } from '../api'

const upstreams = ref([])
const selectedUpstream = ref('')
const libraries = ref([])
const selectedLibrary = ref('')
const search = ref('')
const items = ref([])
const total = ref(0)
const page = ref(0)
const limit = 50
const loading = ref(false)
const error = ref('')

const detail = ref(null)
const detailLoading = ref(false)
const resolveResult = ref(null)
const resolveError = ref('')

async function loadUpstreams() {
  try {
    const payload = await api.upstreams()
    upstreams.value = (payload.upstreams || []).filter((upstream) => upstream.active && upstream.hasApiKey)
    if (upstreams.value.length && !selectedUpstream.value) {
      selectedUpstream.value = upstreams.value[0].name
    }
  } catch (loadError) {
    error.value = loadError.message
  }
}

async function loadLibraries() {
  libraries.value = []
  selectedLibrary.value = ''
  if (!selectedUpstream.value) return
  try {
    const payload = await api.libraries(selectedUpstream.value)
    libraries.value = payload.libraries || []
    if (libraries.value.length) {
      selectedLibrary.value = libraries.value[0].id
    }
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
  }
}

async function loadItems() {
  if (!selectedUpstream.value) return
  loading.value = true
  try {
    const payload = await api.items(selectedUpstream.value, {
      libraryId: selectedLibrary.value,
      limit,
      page: page.value,
      search: search.value.trim()
    })
    items.value = payload.items || []
    total.value = payload.total || items.value.length
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
    items.value = []
  } finally {
    loading.value = false
  }
}

async function openItem(item) {
  detailLoading.value = true
  detail.value = null
  resolveResult.value = null
  resolveError.value = ''
  try {
    detail.value = await api.itemFiles(selectedUpstream.value, item.id)
  } catch (openError) {
    error.value = openError.message
  } finally {
    detailLoading.value = false
  }
}

async function resolveFile(file) {
  resolveResult.value = null
  resolveError.value = ''
  try {
    resolveResult.value = await api.resolve(selectedUpstream.value, {
      kind: 'library-file',
      itemId: detail.value.item.id,
      fileId: file.id,
      mediaSourceId: file.id
    })
  } catch (error_) {
    resolveError.value = error_.message
  }
}

function playUrl(file) {
  return `${window.location.origin}${file.playPath}`
}

watch(selectedUpstream, async () => {
  page.value = 0
  await loadLibraries()
  await loadItems()
})
watch(selectedLibrary, () => {
  page.value = 0
  loadItems()
})

onMounted(async () => {
  await loadUpstreams()
  await loadLibraries()
  await loadItems()
})
</script>

<template>
  <section>
    <div class="panel">
      <h2>书库浏览</h2>
      <div class="grid cols-2">
        <label class="field">
          <span>上游服务</span>
          <select v-model="selectedUpstream">
            <option v-for="upstream in upstreams" :key="upstream.name" :value="upstream.name">
              {{ upstream.name }}（{{ upstream.type }}）
            </option>
          </select>
        </label>
        <label class="field">
          <span>媒体库</span>
          <select v-model="selectedLibrary">
            <option v-for="library in libraries" :key="library.id" :value="library.id">
              {{ library.name }}
            </option>
          </select>
        </label>
      </div>
      <div class="row">
        <input v-model="search" placeholder="按书名搜索" style="max-width:320px" @keyup.enter="page = 0; loadItems()" />
        <button @click="page = 0; loadItems()">搜索</button>
        <button :disabled="page === 0" @click="page--; loadItems()">上一页</button>
        <button :disabled="(page + 1) * limit >= total" @click="page++; loadItems()">下一页</button>
        <span class="muted">共 {{ total }} 项，第 {{ page + 1 }} 页</span>
      </div>
      <p v-if="error" class="error">{{ error }}</p>
    </div>

    <div class="panel">
      <div class="scroll">
        <table>
          <thead>
            <tr>
              <th>标题</th>
              <th>作者</th>
              <th>文件</th>
              <th>STRM</th>
              <th>时长</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in items" :key="item.id" class="clickable" @click="openItem(item)">
              <td>{{ item.title }}</td>
              <td class="muted">{{ item.author || '-' }}</td>
              <td>{{ item.numFiles }}</td>
              <td>
                <span :class="item.numStrm ? 'tag ok' : 'tag'">{{ item.numStrm }}</span>
              </td>
              <td class="mono">{{ item.duration ? Math.round(item.duration / 60) + ' 分' : '-' }}</td>
            </tr>
            <tr v-if="!items.length && !loading">
              <td colspan="5" class="muted">没有条目。</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div class="panel" v-if="detailLoading || detail">
      <h2 v-if="detail">{{ detail.item.title }} · 文件与 STRM 指针</h2>
      <p v-if="detailLoading" class="muted">加载中…</p>
      <div class="scroll" v-if="detail">
        <table>
          <thead>
            <tr>
              <th>#</th>
              <th>文件名</th>
              <th>类型</th>
              <th>解析结果</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="file in detail.files" :key="file.id">
              <td>{{ file.index }}</td>
              <td class="mono">{{ file.filename }}</td>
              <td>
                <span :class="file.isStrm ? 'tag ok' : 'tag'">{{ file.isStrm ? 'strm' : file.ext }}</span>
              </td>
              <td class="mono">
                <div v-if="file.target">
                  <span class="tag">{{ file.target.kind }}</span>
                  {{ file.target.url || file.target.path }}
                </div>
                <div v-else-if="file.error" class="error">{{ file.error }}</div>
                <div v-else class="muted">{{ file.path }}</div>
              </td>
              <td>
                <div class="row">
                  <button @click="resolveFile(file)">验证 302</button>
                  <a :href="playUrl(file)" target="_blank" rel="noreferrer">
                    <button>打开播放地址</button>
                  </a>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="resolveError" class="error">{{ resolveError }}</div>
      <div class="metric" v-if="resolveResult" style="margin-top:12px">
        <div class="label">解析结果</div>
        <div class="row" style="margin:8px 0">
          <span class="tag" :class="resolveResult.willRedirect ? 'ok' : 'warn'">
            {{ resolveResult.willRedirect ? '将返回 302' : '将中继转发' }}
          </span>
          <span class="tag" v-if="resolveResult.cacheHit">缓存命中</span>
          <span class="tag" v-if="resolveResult.isStrm === false">非 STRM 文件</span>
        </div>
        <div class="mono">{{ resolveResult.playUrl || resolveResult.message }}</div>
        <div class="mono muted" v-if="resolveResult.resolution?.hops?.length">
          跳转链：{{ resolveResult.resolution.hops.join(' → ') }}
        </div>
        <div class="mono muted" v-if="resolveResult.resolution">
          容器路径：{{ resolveResult.resolution.containerPath }}
        </div>
      </div>
    </div>
  </section>
</template>
