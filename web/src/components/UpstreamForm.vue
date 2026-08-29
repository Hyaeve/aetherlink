<script setup>
import { computed, ref } from 'vue'
import { api } from '../api'

const props = defineProps({
  // upstream 为 null 表示新增。
  upstream: { type: Object, default: null }
})
const emit = defineEmits(['close', 'saved'])

const isCreate = computed(() => !props.upstream)

function initialForm() {
  const source = props.upstream
  if (!source) {
    return {
      name: '',
      type: 'audiobookshelf',
      baseUrl: '',
      apiKey: '',
      keepApiKey: false,
      enabled: true,
      prefix: '/',
      insecureSkipVerify: false,
      strmRoots: '',
      pathMappings: [{ from: '', to: '' }]
    }
  }
  return {
    name: source.name,
    type: source.type,
    baseUrl: source.baseUrl,
    apiKey: '',
    // 密钥从不回显：已保存时留空即表示保留原值。
    keepApiKey: source.hasApiKey,
    enabled: source.enabled,
    prefix: source.prefix || '/',
    insecureSkipVerify: source.insecureSkipVerify,
    strmRoots: (source.strmRoots || []).join('\n'),
    pathMappings: (source.pathMappings || []).length
      ? source.pathMappings.map((mapping) => ({ ...mapping }))
      : [{ from: '', to: '' }]
  }
}

const form = ref(initialForm())
const busy = ref(false)
const error = ref('')
const testResult = ref(null)

function addMapping() {
  form.value.pathMappings.push({ from: '', to: '' })
}

function removeMapping(index) {
  form.value.pathMappings.splice(index, 1)
  if (!form.value.pathMappings.length) addMapping()
}

function buildPayload() {
  const current = form.value
  const payload = {
    name: current.name.trim(),
    type: current.type,
    baseUrl: current.baseUrl.trim(),
    enabled: current.enabled,
    prefix: current.prefix.trim() || '/',
    insecureSkipVerify: current.insecureSkipVerify,
    strmRoots: current.strmRoots
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean),
    pathMappings: current.pathMappings
      .map((mapping) => ({ from: mapping.from.trim(), to: mapping.to.trim() }))
      .filter((mapping) => mapping.from || mapping.to)
  }
  const key = current.apiKey.trim()
  if (key) {
    payload.apiKey = key
  } else if (!current.keepApiKey) {
    payload.apiKey = ''
  }
  return payload
}

async function test() {
  error.value = ''
  testResult.value = { loading: true }
  try {
    testResult.value = await api.testUpstream(buildPayload())
  } catch (testError) {
    testResult.value = { ok: false, error: testError.message }
  }
}

async function save() {
  error.value = ''
  busy.value = true
  try {
    const payload = buildPayload()
    if (isCreate.value) {
      await api.createUpstream(payload)
    } else {
      await api.updateUpstream(props.upstream.name, payload)
    }
    emit('saved')
  } catch (saveError) {
    error.value = saveError.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="modal-backdrop" @click.self="emit('close')">
    <div class="modal">
      <div class="modal-head">
        <h2>{{ isCreate ? '添加反代上游' : `编辑 ${props.upstream.name}` }}</h2>
        <span class="tag" v-if="!isCreate && props.upstream.active">运行中</span>
        <button class="ghost close" @click="emit('close')">关闭</button>
      </div>

      <div class="modal-body">
        <div class="field-group">
          <div class="title">基本信息</div>
          <div class="hint">名称仅用于识别，不能含斜杠；地址填上游服务的完整 http(s) 地址。</div>
          <div class="grid cols-2">
            <label class="field">
              <span>名称</span>
              <input v-model="form.name" placeholder="abs" />
            </label>
            <label class="field">
              <span>服务端类型</span>
              <select v-model="form.type">
                <option value="audiobookshelf">Audiobookshelf</option>
                <option value="emby">Emby</option>
              </select>
            </label>
            <label class="field">
              <span>上游地址</span>
              <input v-model="form.baseUrl" placeholder="http://10.0.0.31:13378" />
            </label>
            <label class="field">
              <span>反代入口前缀（同时反代多个服务时用来区分，单个填 /）</span>
              <input v-model="form.prefix" placeholder="/" />
            </label>
          </div>
          <div class="row">
            <label class="inline"><input type="checkbox" v-model="form.enabled" /> 启用</label>
            <label class="inline">
              <input type="checkbox" v-model="form.insecureSkipVerify" />
              跳过 TLS 证书校验（自签证书才需要）
            </label>
          </div>
        </div>

        <div class="field-group">
          <div class="title">API 密钥</div>
          <div class="hint">
            Audiobookshelf：设置 → 用户 → 该用户的 API Token。密钥等同于该账号权限，AetherLink 用它查询书库与文件真实路径。
            <template v-if="form.keepApiKey">当前已保存一枚密钥，留空即保留原值。</template>
          </div>
          <input v-model="form.apiKey" type="password" autocomplete="off"
                 :placeholder="form.keepApiKey ? '留空保留原密钥' : '粘贴 API 密钥'" />
        </div>

        <div class="field-group">
          <div class="title">STRM 允许根目录</div>
          <div class="hint">
            每行一个。仅在 .strm 指向容器内本地文件时用到，作为白名单防止指针变成任意文件读取入口；
            指向 http 直链时留空即可。
          </div>
          <textarea v-model="form.strmRoots" rows="3" placeholder="/NetDisk"></textarea>
        </div>

        <div class="field-group">
          <div class="title">路径映射</div>
          <div class="hint">
            把上游报告的媒体路径改写成 AetherLink 容器内的路径。两边挂载同名时不用填。
          </div>
          <div class="row" v-for="(mapping, index) in form.pathMappings" :key="index" style="margin-bottom:8px">
            <input v-model="mapping.from" placeholder="上游看到的路径，如 /audiobooks" style="flex:1;min-width:190px" />
            <span class="muted">→</span>
            <input v-model="mapping.to" placeholder="容器内路径，如 /NetDisk/115-Strm/Set/Read" style="flex:1;min-width:190px" />
            <button class="ghost danger" @click="removeMapping(index)">删除</button>
          </div>
          <button class="ghost" @click="addMapping">增加一条映射</button>
        </div>

        <div class="row" v-if="testResult">
          <span v-if="testResult.loading" class="tag">连接中…</span>
          <template v-else-if="testResult.ok">
            <span class="tag ok">连接成功 · {{ testResult.info }}</span>
            <span class="tag" v-if="testResult.libraries?.length">
              书库 {{ testResult.libraries.map((library) => library.name).join('，') }}
            </span>
            <span class="tag warn" v-if="testResult.warning">{{ testResult.warning }}</span>
          </template>
          <span v-else class="tag bad">{{ testResult.error }}</span>
        </div>
      </div>

      <div class="modal-foot">
        <button :disabled="testResult?.loading" @click="test">试连</button>
        <span v-if="error" class="error">{{ error }}</span>
        <div class="right">
          <button @click="emit('close')">取消</button>
          <button class="primary" :disabled="busy" @click="save">{{ busy ? '保存中…' : '保存并生效' }}</button>
        </div>
      </div>
    </div>
  </div>
</template>
