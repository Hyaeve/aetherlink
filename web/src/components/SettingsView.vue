<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'

defineProps({ status: { type: Object, default: null } })
const emit = defineEmits(['saved', 'account-changed'])

const settings = ref(null)
const server = ref(null)
const account = ref(null)
const error = ref('')
const saved = ref(false)
const busy = ref(false)

const username = ref('')
const currentPassword = ref('')
const newPassword = ref('')
const newPasswordAgain = ref('')
const accountError = ref('')
const accountBusy = ref(false)

async function load() {
  try {
    const payload = await api.config()
    settings.value = payload.settings
    server.value = payload.server
    account.value = payload.account
    username.value = payload.account?.username || ''
    error.value = ''
  } catch (loadError) {
    error.value = loadError.message
  }
}

async function save() {
  busy.value = true
  saved.value = false
  error.value = ''
  try {
    const payload = await api.saveSettings(settings.value)
    settings.value = payload.settings
    saved.value = true
    emit('saved')
  } catch (saveError) {
    error.value = saveError.message
  } finally {
    busy.value = false
  }
}

async function saveAccount() {
  accountError.value = ''
  if (newPassword.value && newPassword.value !== newPasswordAgain.value) {
    accountError.value = '两次输入的新密码不一致'
    return
  }
  accountBusy.value = true
  try {
    await api.updateAccount(currentPassword.value, username.value, newPassword.value)
    currentPassword.value = ''
    newPassword.value = ''
    newPasswordAgain.value = ''
    emit('account-changed')
  } catch (saveError) {
    accountError.value = saveError.message
  } finally {
    accountBusy.value = false
  }
}

onMounted(load)
</script>

<template>
  <section>
    <p v-if="error" class="error">{{ error }}</p>

    <div class="panel" v-if="settings">
      <h2>302 跳转策略</h2>
      <div class="grid cols-2">
        <label class="field">
          <span>跳转模式</span>
          <select v-model="settings.redirect.mode">
            <option value="always">always · 解析成功就 302（推荐）</option>
            <option value="private">private · 仅内网地址 302，公网地址中继</option>
            <option value="never">never · 从不 302，全部由 AetherLink 中继</option>
          </select>
        </label>
        <label class="field">
          <span>回落 User-Agent（播放器未带 UA 时使用）</span>
          <input v-model="settings.redirect.fallbackUserAgent" />
        </label>
        <label class="field">
          <span>探测超时（如 15s）</span>
          <input v-model="settings.redirect.probeTimeout" />
        </label>
        <label class="field">
          <span>中继超时（0 表示不限制，媒体长连接建议留 0）</span>
          <input v-model="settings.redirect.streamTimeout" />
        </label>
        <label class="field">
          <span>最多跟随几跳</span>
          <input v-model.number="settings.redirect.maxFollowHops" type="number" min="1" />
        </label>
      </div>
      <div class="row">
        <label class="inline">
          <input type="checkbox" v-model="settings.redirect.followUpstreamRedirects" />
          先跟随上游 302，把最终地址交给播放器（115 pick code 这类签名直链常需要）
        </label>
      </div>
      <div class="row" style="margin-top:8px">
        <label class="inline">
          <input type="checkbox" v-model="settings.redirect.forwardUserAgent" />
          转发播放器 User-Agent
        </label>
        <label class="inline">
          <input type="checkbox" v-model="settings.redirect.allowPublicTargets" />
          允许 302 到公网地址
        </label>
      </div>
    </div>

    <div class="panel" v-if="settings">
      <h2>缓存与日志</h2>
      <div class="grid cols-2">
        <label class="field">
          <span>解析缓存 TTL（如 5m，0 表示不缓存）</span>
          <input v-model="settings.cache.ttl" />
        </label>
        <label class="field">
          <span>缓存条目上限</span>
          <input v-model.number="settings.cache.maxSize" type="number" min="1" />
        </label>
        <label class="field">
          <span>日志级别</span>
          <select v-model="settings.logLevel">
            <option value="debug">debug</option>
            <option value="info">info</option>
            <option value="warn">warn</option>
            <option value="error">error</option>
          </select>
        </label>
        <label class="field">
          <span>界面保留日志条数</span>
          <input v-model.number="settings.logBuffer" type="number" min="50" />
        </label>
      </div>
      <div class="row">
        <button class="primary" :disabled="busy" @click="save">{{ busy ? '保存中…' : '保存并生效' }}</button>
        <span v-if="saved" class="tag ok">已保存，立即生效</span>
      </div>
    </div>

    <div class="panel">
      <h2>管理账号</h2>
      <p v-if="account?.defaultCredentials" class="notice" style="margin-top:0">
        仍在使用默认账号 admin / password，请尽快修改。
      </p>
      <div class="grid cols-2">
        <label class="field">
          <span>账号</span>
          <input v-model="username" autocomplete="username" />
        </label>
        <label class="field">
          <span>当前密码</span>
          <input v-model="currentPassword" type="password" autocomplete="current-password" />
        </label>
        <label class="field">
          <span>新密码（只改账号时留空）</span>
          <input v-model="newPassword" type="password" autocomplete="new-password" />
        </label>
        <label class="field">
          <span>再输入一次新密码</span>
          <input v-model="newPasswordAgain" type="password" autocomplete="new-password" />
        </label>
      </div>
      <div class="row">
        <button :disabled="accountBusy" @click="saveAccount">
          {{ accountBusy ? '提交中…' : '保存账号' }}
        </button>
        <span class="muted" style="font-size:12px">改完会注销所有登录会话，需要重新登录。</span>
        <span v-if="accountError" class="error">{{ accountError }}</span>
      </div>
    </div>

    <div class="panel" v-if="server">
      <h2>运行信息</h2>
      <div class="grid cols-2">
        <div>
          <div class="muted" style="font-size:12px">配置文件</div>
          <div class="mono">{{ server.configPath }}</div>
        </div>
        <div>
          <div class="muted" style="font-size:12px">管理界面端口</div>
          <div class="mono">{{ server.adminPort || server.listen }}</div>
        </div>
      </div>
      <div class="row" style="margin-top:12px">
        <span class="tag" v-if="status">v{{ status.version }}</span>
        <span class="tag" v-if="status">活跃会话 {{ status.sessions }}</span>
        <span class="tag" v-if="account?.username">账号 {{ account.username }}</span>
        <span class="tag warn" v-if="server.breakGlassEnabled">应急令牌已启用，排障完成后请移除环境变量</span>
        <span class="tag bad" v-if="server.restartRequired">监听地址已改动，需重启容器</span>
      </div>
      <p class="muted" style="font-size:12px;margin-bottom:0">
        管理界面固定在容器内 5151；每个反代上游还会各自占一个容器内端口。
        这些端口都要在 docker compose 的 ports 里映射出去，容器才能被外部访问。
        <template v-if="server.suggestedPort">下一个空闲的反代端口是 {{ server.suggestedPort }}。</template>
      </p>
    </div>
  </section>
</template>
