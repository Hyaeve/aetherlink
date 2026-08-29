<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'

const samples = [
  'http://10.0.0.31:19527/d/bi6jeznun2rvu88v6.m4a?/001.总序.m4a',
  'http://10.0.0.31:25244/d/移动云盘/移动云资源/电视剧/电视剧集/白色巨塔 (2003)/白色巨塔 (2003) S01E01.再读.mkv',
  'http://10.0.0.31:25244/d/139-0211/%E6%9C%89%E5%A3%B0%E8%AF%BB%E7%89%A9/0001-0500/001.m4a',
  '/NetDisk/CloudNAS/CloudDrive/book/001.flac'
]

const content = ref(samples[0])
const basePath = ref('')
const upstream = ref('')
const upstreams = ref([])
const result = ref(null)
const error = ref('')
const loading = ref(false)

async function parse() {
  loading.value = true
  error.value = ''
  result.value = null
  try {
    result.value = await api.parseStrm({
      content: content.value,
      basePath: basePath.value.trim(),
      upstream: upstream.value
    })
  } catch (parseError) {
    error.value = parseError.message
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  try {
    const payload = await api.upstreams()
    upstreams.value = payload.upstreams || []
  } catch {
    // Upstream list is optional here: raw parsing works without one.
  }
})
</script>

<template>
  <section>
    <div class="panel">
      <h2>STRM 实验室</h2>
      <p class="muted">
        粘贴 <code>.strm</code> 文件内容，检查 AetherLink 会如何归一化 URL、识别指针类型并生成 302 目标。
        选择上游后会同时套用该上游的路径映射与根目录白名单。
      </p>

      <label class="field">
        <span>指针内容</span>
        <textarea v-model="content" rows="4" class="mono"></textarea>
      </label>

      <div class="grid cols-2">
        <label class="field">
          <span>所属 .strm 文件路径（解析相对路径时需要）</span>
          <input v-model="basePath" class="mono" placeholder="/NetDisk/115-Strm/书名/001.strm" />
        </label>
        <label class="field">
          <span>套用上游的路径规则（可选）</span>
          <select v-model="upstream">
            <option value="">不套用</option>
            <option v-for="item in upstreams" :key="item.name" :value="item.name">{{ item.name }}</option>
          </select>
        </label>
      </div>

      <div class="row">
        <button class="primary" :disabled="loading" @click="parse">解析</button>
        <button v-for="(sample, index) in samples" :key="index" @click="content = sample">示例 {{ index + 1 }}</button>
      </div>
      <p v-if="error" class="error">{{ error }}</p>
    </div>

    <div class="panel" v-if="result">
      <h2>解析结果</h2>
      <div v-if="result.ok">
        <div class="row" style="margin-bottom:12px">
          <span class="tag ok">{{ result.target.type === 'remote' ? '远程 URL' : '容器本地文件' }}</span>
          <span class="tag">{{ result.target.kind }}</span>
        </div>
        <table>
          <tbody>
            <tr>
              <th>归一化目标</th>
              <td class="mono">{{ result.target.url || result.target.path }}</td>
            </tr>
            <tr>
              <th>显示文件名</th>
              <td class="mono">{{ result.target.filename }}</td>
            </tr>
            <tr>
              <th>原始内容</th>
              <td class="mono muted">{{ result.target.raw }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else class="error">{{ result.error }}</p>
    </div>
  </section>
</template>
