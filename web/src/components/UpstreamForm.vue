<script setup>
import { computed, ref } from 'vue'
import { api } from '../api'

const props = defineProps({
  // upstream 为 null 表示新增。
  upstream: { type: Object, default: null },
  // 新增时预填的空闲端口，由列表页从 /upstreams 带过来。
  suggestedPort: { type: Number, default: 0 },
  adminPort: { type: Number, default: 0 }
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
      listenPort: props.suggestedPort || null,
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
    listenPort: source.listenPort || null,
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

const isEmby = computed(() => form.value.type === 'emby')

// 从上游地址里取出端口，用来在提示里写出「原端口 → 反代端口」。
const sourcePort = computed(() => {
  try {
    const parsed = new URL(form.value.baseUrl)
    return parsed.port || (parsed.protocol === 'https:' ? '443' : '80')
  } catch {
    return ''
  }
})

const keyHint = computed(() =>
  isEmby.value
    ? 'Emby 控制台 → 高级 → API 密钥 → 新建 API 密钥，把生成的字符串粘到这里。'
    : 'Audiobookshelf 后台 → 设置 → 用户 → 点开该用户 → API Token。'
)

const keyPlaceholder = computed(() => {
  if (form.value.keepApiKey) return '留空保留原密钥'
  return isEmby.value ? '粘贴 Emby API 密钥' : '粘贴 Audiobookshelf API Token'
})

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
    listenPort: Number(current.listenPort) || 0,
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
        <span class="tag" v-if="!isCreate && props.upstream.listening">端口已监听</span>
        <button class="ghost close" @click="emit('close')">关闭</button>
      </div>

      <div class="modal-body">
        <div class="field-group">
          <div class="title">基本信息</div>
          <div class="hint">
            反代端口是 AetherLink 在容器内为这个上游单独开的端口：播放端把地址里的
            <template v-if="sourcePort">{{ sourcePort }}</template><template v-else>上游端口</template>
            换成它，路径保持原样。端口要在 docker compose 的 ports 里一并映射出去。
          </div>
          <div class="grid cols-2">
            <label class="field">
              <span>名称（仅用于识别，不能含斜杠）</span>
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
              <input v-model="form.baseUrl" :placeholder="isEmby ? 'http://10.0.0.31:8096' : 'http://10.0.0.31:13378'" />
            </label>
            <label class="field">
              <span>反代端口</span>
              <input v-model.number="form.listenPort" type="number" min="1" max="65535"
                     :placeholder="props.suggestedPort ? String(props.suggestedPort) : '如 5152'" />
            </label>
          </div>
          <div class="row">
            <label class="inline"><input type="checkbox" v-model="form.enabled" /> 启用</label>
            <label class="inline">
              <input type="checkbox" v-model="form.insecureSkipVerify" />
              跳过 TLS 证书校验（自签证书才需要）
            </label>
            <span class="muted" style="font-size:12px" v-if="props.adminPort">
              管理端口 {{ props.adminPort }} 已占用，不能复用。
            </span>
          </div>
        </div>

        <div class="field-group">
          <div class="title">{{ isEmby ? 'Emby API 密钥' : 'Audiobookshelf API 密钥' }}</div>
          <div class="hint">
            {{ keyHint }}
            密钥等同于该账号权限，AetherLink 用它查询媒体库与文件真实路径。
            <template v-if="form.keepApiKey">当前已保存一枚密钥，留空即保留原值。</template>
          </div>
          <input v-model="form.apiKey" type="password" autocomplete="off" :placeholder="keyPlaceholder" />
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
            <input v-model="mapping.from" :placeholder="isEmby ? '上游看到的路径，如 /media' : '上游看到的路径，如 /audiobooks'" style="flex:1;min-width:190px" />
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
              媒体库 {{ testResult.libraries.map((library) => library.name).join('，') }}
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