<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api, visibleMessage } from '../api'

const props = defineProps({
  // upstream 为 null 表示新增。
  upstream: { type: Object, default: null },
  // 新增时预填的空闲端口，由列表页从 /upstreams 带过来。
  suggestedPort: { type: Number, default: 0 }
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
      username: '',
      password: '',
      keepPassword: false,
      enabled: true,
      listenPort: props.suggestedPort || null,
      insecureSkipVerify: false,
      strmRoots: '',
      pathMappings: [{ from: '', to: '' }],
      redirectMode: 'always'
    }
  }
  return {
    name: source.name,
    type: source.type,
    baseUrl: source.baseUrl,
    apiKey: '',
    // 先留空并标记「保留原值」，onMounted 里再把已保存的密钥取回来填上。
    // 万一取不回来，这个标记继续生效，空字段就不会被误当成「用户想清空」。
    keepApiKey: source.hasApiKey,
    // 账号本来就不是秘密，直接回显；密码和密钥一样先留空，等取回来填。
    username: source.username || '',
    password: '',
    keepPassword: Boolean(source.hasPassword),
    enabled: source.enabled,
    listenPort: source.listenPort || null,
    insecureSkipVerify: source.insecureSkipVerify,
    strmRoots: (source.strmRoots || []).join('\n'),
    pathMappings: (source.pathMappings || []).length
      ? source.pathMappings.map((mapping) => ({ ...mapping }))
      : [{ from: '', to: '' }],
    redirectMode: source.redirectMode || 'always'
  }
}

const form = ref(initialForm())
const busy = ref(false)
const error = ref('')
const testResult = ref(null)
const editor = ref(null)
// 两个秘密字段都默认遮蔽成一串圆点，点输入框右边的小眼睛才切明文：
// 未显示时是普通眼睛（点一下显示），已显示时是斜线眼睛（再点回圆点）。
// 密钥和口令一视同仁——不因为密钥「通常不算口令」就摊在屏幕上。
const keyVisible = ref(false)
const passwordVisible = ref(false)
// 取回已保存秘密的那一小会儿把字段锁住，否则请求返回时会盖掉用户刚敲进去的字。
const secretsLoading = ref(false)

const serviceOptions = [
  { value: 'audiobookshelf', label: 'Audiobookshelf' },
  { value: 'emby', label: 'Emby' },
  { value: 'fnos', label: '飞牛影视' }
]
// 每种服务端的地址示例、密钥/账号提示与路径示例。
const serviceHints = {
  audiobookshelf: {
    address: 'http://10.0.0.31:13378',
    key: '粘贴 Audiobookshelf API Token',
    mapping: '上游看到的路径，如 /audiobooks'
  },
  emby: {
    address: 'http://10.0.0.31:8096',
    key: '粘贴 Emby API 密钥',
    mapping: '上游看到的路径，如 /media'
  },
  // 飞牛影视没有 API 密钥，改用客户端账号密码登录换令牌。
  fnos: {
    address: 'http://10.0.0.31:8005',
    account: '飞牛影视登录账号',
    password: '飞牛影视登录密码',
    mapping: '上游看到的路径，如 /media'
  }
}
const serviceHint = computed(() => serviceHints[form.value.type] || serviceHints.emby)
// 飞牛影视没有静态 API 密钥，表单里为它换成账号密码两项。
const showApiKey = computed(() => form.value.type !== 'fnos')
const showCredentials = computed(() => form.value.type === 'fnos')
const redirectOptions = [
  { value: 'always', label: '始终跳转' },
  { value: 'public', label: '公网跳转' },
  { value: 'private', label: '内网跳转' },
  { value: 'never', label: '始终中继' }
]

const keyPlaceholder = computed(() => {
  if (form.value.keepApiKey) return '留空保留原密钥'
  return serviceHint.value.key
})

const passwordPlaceholder = computed(() => {
  if (form.value.keepPassword) return '留空保留原密码'
  return serviceHint.value.password
})

// 已保存的密钥/密码在弹窗打开时就取回来填进字段，并保持遮蔽（圆点），
// 用户点输入框右侧的小眼睛才看明文。列表接口仍然一个秘密都不给，所以这里必须
// 单独请求一次 /credentials；取回来的值就原样留在表单里，保存时送回，值不变。
//
// 取不回来时退回「留空即保留原值」的老语义（keepXxx 保持为真）：否则一次网络
// 抖动就会让空字段被当成「用户想清空」，把配置里的密钥悄悄抹掉。
async function loadSavedSecrets() {
  const summary = props.upstream
  if (!summary) return
  const wantsPassword = form.value.type === 'fnos'
  if (wantsPassword ? !summary.hasPassword : !summary.hasApiKey) return
  secretsLoading.value = true
  try {
    const credentials = await api.upstreamCredentials(summary.name)
    if (wantsPassword) {
      form.value.password = credentials?.password || ''
      form.value.keepPassword = !form.value.password
      return
    }
    form.value.apiKey = (credentials?.apiKey || '').trim()
    form.value.keepApiKey = !form.value.apiKey
  } catch (loadError) {
    // 会话失效已经由 api 层处理成回登录页，不必再挂一句「读取凭据失败」。
    if (!loadError.sessionExpired) {
      error.value = `读取已保存的凭据失败，直接保存会保留原值：${loadError.message}`
    }
  } finally {
    secretsLoading.value = false
  }
}

// 飞牛影视的账号与密码必须成对。后端会拒掉「只有一半」的配置，这里先给一句
// 更直白的提示，免得用户对着 400 猜。清空账号等于删除凭据，是允许的，
// 但那时不能再留着密码。
function credentialError() {
  if (!showCredentials.value) return ''
  const username = form.value.username.trim()
  const password = form.value.password
  if (username === '') {
    return password ? '请填写飞牛影视的登录账号，或把密码也一起清空' : ''
  }
  if (!password && !form.value.keepPassword) return '请填写飞牛影视的登录密码'
  return ''
}

function addMapping() {
  form.value.pathMappings.push({ from: '', to: '' })
}

function removeMapping(index) {
  form.value.pathMappings.splice(index, 1)
  if (!form.value.pathMappings.length) addMapping()
}

function optionLabel(options, value) {
  return options.find((option) => option.value === value)?.label || ''
}

function selectOption(field, value, event) {
  form.value[field] = value
  const dropdown = event.currentTarget.closest('details')
  dropdown?.removeAttribute('open')
  dropdown?.querySelector('summary')?.focus()
}

function closeDropdowns(event) {
  editor.value?.querySelectorAll('.form-select[open]').forEach((dropdown) => {
    if (!dropdown.contains(event.target)) dropdown.removeAttribute('open')
  })
}

function handleDropdownKey(event) {
  const dropdown = event.currentTarget
  if (event.key === 'Escape') {
    event.preventDefault()
    event.stopPropagation()
    dropdown.removeAttribute('open')
    dropdown.querySelector('summary')?.focus()
    return
  }
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  dropdown.open = true
  const options = [...dropdown.querySelectorAll('button')]
  const current = options.indexOf(document.activeElement)
  let next = current < 0 ? options.findIndex((option) => option.classList.contains('selected')) : current
  if (event.key === 'Home') next = 0
  else if (event.key === 'End') next = options.length - 1
  else if (current >= 0) next = (current + (event.key === 'ArrowDown' ? 1 : -1) + options.length) % options.length
  options[Math.max(0, next)]?.focus()
}

onMounted(() => {
  document.addEventListener('pointerdown', closeDropdowns)
  document.addEventListener('focusin', closeDropdowns)
  loadSavedSecrets()
})
onUnmounted(() => {
  document.removeEventListener('pointerdown', closeDropdowns)
  document.removeEventListener('focusin', closeDropdowns)
})

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
      .filter((mapping) => mapping.from || mapping.to),
    redirectMode: current.redirectMode
  }
  // 飞牛影视没有 API 密钥：表单里不显示这个输入框，这里也整段跳过，
  // 免得把配置文件里手工补过的 api_key 清掉。
  if (current.type !== 'fnos') {
    const key = current.apiKey.trim()
    if (key) {
      payload.apiKey = key
    } else if (!current.keepApiKey) {
      payload.apiKey = ''
    }
  } else {
    // 飞牛影视改用账号密码登录换令牌。两项必须成对：清空账号就表示要删掉
    // 凭据，此时把密码一并清掉，否则后端会因为「只有一半」拒绝保存。
    const username = current.username.trim()
    payload.username = username
    if (username === '') {
      payload.password = ''
    } else if (current.password) {
      payload.password = current.password
    } else if (!current.keepPassword) {
      payload.password = ''
    }
  }
  return payload
}

// 飞牛影视的试连必须带上登录凭据：它没有静态密钥，未登录时上游连服务信息都
// 不给，试连必然失败 —— 那种失败说明不了地址对不对，等于白试。已经保存过密码
// 时字段留空是允许的（后端会沿用原值），所以「已保存」也算作有密码。
function testCredentialError() {
  if (!showCredentials.value) return ''
  if (!form.value.username.trim()) {
    return '飞牛影视需要先填写登录账号与密码：它没有静态密钥，未登录无法连接'
  }
  if (!form.value.password && !props.upstream?.hasPassword) {
    return '飞牛影视需要先填写登录密码，否则试连没有意义'
  }
  return ''
}

async function test() {
  error.value = credentialError() || testCredentialError()
  if (error.value) return
  testResult.value = { loading: true }
  try {
    testResult.value = await api.testUpstream(buildPayload())
  } catch (testError) {
    // 会话失效已经回登录页了，别在结果框里留一条空错误。
    testResult.value = testError.sessionExpired ? null : { ok: false, error: testError.message }
  }
}

async function save() {
  error.value = credentialError()
  if (error.value) return
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
    error.value = visibleMessage(saveError)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="modal-backdrop" @click.self="emit('close')">
    <div ref="editor" class="modal">
      <div class="modal-head">
        <h2>{{ isCreate ? '添加反代上游' : `编辑 ${props.upstream.name}` }}</h2>
        <span class="tag" v-if="!isCreate && props.upstream.listening">端口已监听</span>
        <button class="ghost close" @click="emit('close')">关闭</button>
      </div>

      <div class="modal-body">
        <div class="field-group">
          <div class="title">基本信息</div>
          <div class="grid cols-2">
            <label class="field">
              <span>名称</span>
              <input v-model="form.name" placeholder="例如：我的有声书" />
            </label>
            <div class="field">
              <span>服务端类型</span>
              <details class="form-select" @keydown="handleDropdownKey">
                <summary :aria-label="`服务端类型：${optionLabel(serviceOptions, form.type)}`">{{ optionLabel(serviceOptions, form.type) }}</summary>
                <div class="form-select-options">
                  <button
                    v-for="option in serviceOptions"
                    :key="option.value"
                    type="button"
                    :aria-pressed="form.type === option.value"
                    :class="{ selected: form.type === option.value }"
                    @click="selectOption('type', option.value, $event)"
                  >
                    {{ option.label }}
                  </button>
                </div>
              </details>
            </div>
            <label class="field">
              <span>原服务地址</span>
              <input v-model="form.baseUrl" :placeholder="serviceHint.address" />
            </label>
            <label class="field">
              <span>反代端口</span>
              <input v-model.number="form.listenPort" type="number" min="1" max="65535"
                     :placeholder="props.suggestedPort ? String(props.suggestedPort) : '如 5152'" />
              <small class="field-note">保存后把宿主机端口映射到这个端口</small>
            </label>
            <label class="field" v-if="showApiKey">
              <span>API 密钥</span>
              <span class="secret-input">
                <input
                  v-model="form.apiKey"
                  :type="keyVisible ? 'text' : 'password'"
                  autocomplete="off"
                  spellcheck="false"
                  :disabled="secretsLoading"
                  :placeholder="secretsLoading ? '读取已保存的密钥…' : keyPlaceholder"
                />
                <button
                  type="button"
                  class="secret-toggle"
                  :aria-pressed="keyVisible"
                  :title="keyVisible ? '隐藏密钥' : '显示密钥'"
                  :aria-label="keyVisible ? '隐藏密钥' : '显示密钥'"
                  @click.prevent="keyVisible = !keyVisible"
                >
                  <svg v-if="keyVisible" viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z" />
                    <circle cx="12" cy="12" r="3.1" />
                    <path d="m4 3.6 16 16.8" />
                  </svg>
                  <svg v-else viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z" />
                    <circle cx="12" cy="12" r="3.1" />
                  </svg>
                </button>
              </span>
            </label>
            <label class="field" v-if="showCredentials">
              <span>登录账号</span>
              <input v-model="form.username" autocomplete="off" spellcheck="false" :placeholder="serviceHint.account" />
            </label>
            <label class="field" v-if="showCredentials">
              <span>登录密码</span>
              <span class="secret-input">
                <input
                  v-model="form.password"
                  :type="passwordVisible ? 'text' : 'password'"
                  autocomplete="new-password"
                  spellcheck="false"
                  :disabled="secretsLoading"
                  :placeholder="secretsLoading ? '读取已保存的密码…' : passwordPlaceholder"
                />
                <button
                  type="button"
                  class="secret-toggle"
                  :aria-pressed="passwordVisible"
                  :title="passwordVisible ? '隐藏密码' : '显示密码'"
                  :aria-label="passwordVisible ? '隐藏密码' : '显示密码'"
                  @click.prevent="passwordVisible = !passwordVisible"
                >
                  <svg v-if="passwordVisible" viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z" />
                    <circle cx="12" cy="12" r="3.1" />
                    <path d="m4 3.6 16 16.8" />
                  </svg>
                  <svg v-else viewBox="0 0 24 24" aria-hidden="true">
                    <path d="M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z" />
                    <circle cx="12" cy="12" r="3.1" />
                  </svg>
                </button>
              </span>
            </label>
            <!-- 播放跳转排在账号密码之后：飞牛影视没有 API 密钥这一项，
                 账号密码因此并排落在同一行，跳转选择框跟在它们后面。 -->
            <div class="field">
              <span>播放跳转</span>
              <details class="form-select" @keydown="handleDropdownKey">
                <summary :aria-label="`播放跳转：${optionLabel(redirectOptions, form.redirectMode)}`">{{ optionLabel(redirectOptions, form.redirectMode) }}</summary>
                <div class="form-select-options">
                  <button
                    v-for="option in redirectOptions"
                    :key="option.value"
                    type="button"
                    :aria-pressed="form.redirectMode === option.value"
                    :class="{ selected: form.redirectMode === option.value }"
                    @click="selectOption('redirectMode', option.value, $event)"
                  >
                    {{ option.label }}
                  </button>
                </div>
              </details>
            </div>
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
          <div class="title">STRM 允许根目录</div>
          <textarea v-model="form.strmRoots" rows="3" placeholder="/NetDisk"></textarea>
        </div>

        <div class="field-group">
          <div class="title">路径映射</div>
          <div class="row" v-for="(mapping, index) in form.pathMappings" :key="index" style="margin-bottom:8px">
            <input v-model="mapping.from" :placeholder="serviceHint.mapping" style="flex:1;min-width:190px" />
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
