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
  <section class="settings-page">
    <p v-if="error" class="error settings-error">{{ error }}</p>

    <div v-if="!settings && !error" class="settings-loading">
      <span class="loading-orb"></span>
      <span>正在读取系统设置…</span>
    </div>

    <div v-if="settings" class="settings-layout">
      <aside class="settings-sidebar">
        <section class="settings-card account-card">
          <div class="settings-card-head">
            <div class="settings-icon violet" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <circle cx="8" cy="8" r="3" />
                <path d="M10.5 10.5 20 20" />
                <path d="m16 16 2-2" />
                <path d="m18 18 2-2" />
              </svg>
            </div>
            <div>
              <h2>管理账号</h2>
              <p>更新登录身份与访问口令</p>
            </div>
          </div>

          <div v-if="account?.defaultCredentials" class="setting-alert">
            <span class="alert-dot"></span>
            <span>建议尽快修改默认口令</span>
          </div>

          <div class="account-form">
            <label class="field">
              <span>账号</span>
              <input v-model="username" autocomplete="username" />
            </label>
            <label class="field">
              <span>当前密码</span>
              <input v-model="currentPassword" type="password" autocomplete="current-password" />
            </label>
            <label class="field">
              <span>新密码</span>
              <input v-model="newPassword" type="password" autocomplete="new-password" placeholder="留空则不修改" />
            </label>
            <label class="field">
              <span>确认新密码</span>
              <input v-model="newPasswordAgain" type="password" autocomplete="new-password" />
            </label>
          </div>

          <button class="primary wide-action" :disabled="accountBusy" @click="saveAccount">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M5 4h11l3 3v13H5z" />
              <path d="M8 4v6h8V4" />
              <path d="M8 20v-6h8v6" />
            </svg>
            {{ accountBusy ? '保存中…' : '保存账号' }}
          </button>
          <p v-if="accountError" class="error form-error">{{ accountError }}</p>
          <p class="card-footnote">修改账号后，现有登录会话会失效。</p>
        </section>

        <section class="settings-card runtime-card" v-if="server">
          <div class="settings-card-head compact">
            <div class="settings-icon blue" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <rect x="4" y="4" width="16" height="6" rx="2" />
                <rect x="4" y="14" width="16" height="6" rx="2" />
                <path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" />
              </svg>
            </div>
            <div>
              <h2>运行信息</h2>
              <p>AetherLink 当前状态</p>
            </div>
          </div>
          <div class="runtime-state"><span class="health-dot"></span>服务运行中</div>
          <dl class="runtime-list">
            <div><dt>管理端口</dt><dd>{{ server.adminPort || server.listen }}</dd></div>
            <div><dt>配置文件</dt><dd class="mono">{{ server.configPath }}</dd></div>
            <div><dt>版本</dt><dd>v1.0</dd></div>
            <div v-if="status"><dt>活跃会话</dt><dd>{{ status.sessions }}</dd></div>
          </dl>
          <div class="runtime-tags">
            <span class="tag warn" v-if="server.breakGlassEnabled">应急令牌启用</span>
            <span class="tag bad" v-if="server.restartRequired">需要重启</span>
          </div>
        </section>
      </aside>

      <div class="settings-main">
        <section class="settings-card system-card">
          <div class="settings-card-head system-head">
            <div class="settings-icon indigo" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <path d="m12 3 2.1 5.1L19 10l-4.9 1.9L12 17l-2.1-5.1L5 10l4.9-1.9z" />
                <path d="m19 16 .8 2.2L22 19l-2.2.8L19 22l-.8-2.2L16 19l2.2-.8z" />
              </svg>
            </div>
            <div>
              <h2>缓存与日志</h2>
              <p>管理解析缓存和运行日志</p>
            </div>
            <span class="live-badge"><i></i>实时生效</span>
          </div>

          <div class="settings-section">
            <div class="settings-section-title">
              <span>缓存与日志</span>
              <small>重复播放更快，保留必要的排障记录</small>
            </div>
            <div class="form-grid two">
              <label class="field field-large">
                <span>直链缓存 TTL</span>
                <input v-model="settings.cache.ttl" />
                <small>默认 5h，填 0 表示不缓存</small>
              </label>
              <label class="field field-large">
                <span>缓存条目上限</span>
                <input v-model.number="settings.cache.maxSize" type="number" min="1" />
                <small>超过后自动淘汰最久未使用项</small>
              </label>
              <label class="field field-large">
                <span>日志级别</span>
                <select v-model="settings.logLevel">
                  <option value="debug">debug · 最详细</option>
                  <option value="info">info · 推荐</option>
                  <option value="warn">warn · 仅警告</option>
                  <option value="error">error · 仅错误</option>
                </select>
                <small>运行日志页会同步更新</small>
              </label>
              <label class="field field-large">
                <span>界面保留日志条数</span>
                <input v-model.number="settings.logBuffer" type="number" min="50" />
                <small>用于播放流水与运行日志查询</small>
              </label>
            </div>
          </div>

          <div class="settings-actions">
            <div>
              <strong>保存系统设置</strong>
              <span v-if="saved" class="save-confirm"><i></i>已保存，立即生效</span>
            </div>
            <button class="primary action-button" :disabled="busy" @click="save">
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <path d="M5 4h11l3 3v13H5z" />
                <path d="M8 4v6h8V4" />
                <path d="M8 20v-6h8v6" />
              </svg>
              {{ busy ? '保存中…' : '保存并生效' }}
            </button>
          </div>
        </section>

        <div class="settings-tip">
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <circle cx="12" cy="12" r="9" />
            <path d="M12 11v5M12 8h.01" />
          </svg>
          <span>每个反代上游仍需单独把容器端口映射到宿主机，播放端只更换端口，路径保持不变。</span>
        </div>
      </div>
    </div>
  </section>
</template>
