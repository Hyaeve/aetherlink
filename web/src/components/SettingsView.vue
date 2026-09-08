<script setup>
import { nextTick, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'

defineProps({ status: { type: Object, default: null } })
const emit = defineEmits(['saved', 'account-changed'])

const settings = ref(null)
const server = ref(null)
const account = ref(null)
const upstreams = ref([])
const error = ref('')
const saved = ref(false)
const busy = ref(false)
const securitySaved = ref(false)
const securityBusy = ref(false)

const username = ref('')
const password = ref('')
const accountError = ref('')
const accountBusy = ref(false)
const accountConfirm = ref(false)
const blockedUserAgentText = ref('')
const blockedEmbyUserAgentText = ref('')
const blockedAudiobookshelfUserAgentText = ref('')
const restoreInput = ref(null)
const backupBusy = ref(false)
const accountCard = ref(null)
const systemCard = ref(null)
let cardResizeObserver = null

function alignPrimaryCards() {
  if (!accountCard.value || !systemCard.value) return
  accountCard.value.style.minHeight = ''
  systemCard.value.style.minHeight = ''
  const height = Math.max(accountCard.value.offsetHeight, systemCard.value.offsetHeight)
  accountCard.value.style.minHeight = `${height}px`
  systemCard.value.style.minHeight = `${height}px`
}

function candidateUpstreams(type) {
  return upstreams.value.filter((upstream) => upstream.type === type)
}

function selectedUpstreams(type) {
  const selected = type === 'emby'
    ? settings.value?.redirect?.blockedUserAgentsEmbyUpstreams || []
    : settings.value?.redirect?.blockedUserAgentsAudiobookshelfUpstreams || []
  return candidateUpstreams(type).filter((upstream) => selected.includes(upstream.name))
}

function candidateSummary(type) {
  const names = selectedUpstreams(type).map((upstream) => upstream.name)
  return names.length ? names.join('、') : `选择已添加的 ${type === 'emby' ? 'Emby' : 'ABS'} 服务`
}

function isCandidateSelected(type, name) {
  const key = type === 'emby' ? 'blockedUserAgentsEmbyUpstreams' : 'blockedUserAgentsAudiobookshelfUpstreams'
  return (settings.value?.redirect?.[key] || []).includes(name)
}

function toggleCandidate(type, name) {
  const key = type === 'emby' ? 'blockedUserAgentsEmbyUpstreams' : 'blockedUserAgentsAudiobookshelfUpstreams'
  const selected = settings.value.redirect[key] || []
  settings.value.redirect[key] = selected.includes(name)
    ? selected.filter((value) => value !== name)
    : [...selected, name]
}

function preventCandidateMenu(event, enabled) {
  if (!enabled) event.preventDefault()
}

function syncBlockedUserAgents(settingsPayload) {
  if (!Array.isArray(settingsPayload?.redirect?.blockedUserAgentsEmbyUpstreams)) settingsPayload.redirect.blockedUserAgentsEmbyUpstreams = []
  if (!Array.isArray(settingsPayload?.redirect?.blockedUserAgentsAudiobookshelfUpstreams)) settingsPayload.redirect.blockedUserAgentsAudiobookshelfUpstreams = []
  blockedUserAgentText.value = (settingsPayload?.redirect?.blockedUserAgents || []).join('\n')
  blockedEmbyUserAgentText.value = (settingsPayload?.redirect?.blockedUserAgentsEmby || []).join('\n')
  blockedAudiobookshelfUserAgentText.value = (settingsPayload?.redirect?.blockedUserAgentsAudiobookshelf || []).join('\n')
}

function applySecurityDraft() {
  settings.value.redirect.blockedUserAgents = blockedUserAgentText.value
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
  settings.value.redirect.blockedUserAgentsEmby = blockedEmbyUserAgentText.value
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
  settings.value.redirect.blockedUserAgentsAudiobookshelf = blockedAudiobookshelfUserAgentText.value
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
}

async function load() {
  try {
    const payload = await api.config()
    settings.value = payload.settings
    syncBlockedUserAgents(settings.value)
    server.value = payload.server
    account.value = payload.account
    upstreams.value = payload.upstreams || []
    username.value = payload.account?.username || ''
    error.value = ''
    await nextTick()
    alignPrimaryCards()
    if (!cardResizeObserver && systemCard.value && typeof ResizeObserver !== 'undefined') {
      cardResizeObserver = new ResizeObserver(alignPrimaryCards)
      cardResizeObserver.observe(systemCard.value)
    }
  } catch (loadError) {
    error.value = loadError.message
  }
}

async function save() {
  busy.value = true
  saved.value = false
  error.value = ''
  try {
    applySecurityDraft()
    const payload = await api.saveSettings(settings.value)
    settings.value = payload.settings
    syncBlockedUserAgents(settings.value)
    saved.value = true
    emit('saved')
  } catch (saveError) {
    error.value = saveError.message
  } finally {
    busy.value = false
  }
}

async function saveSecurity() {
  securityBusy.value = true
  securitySaved.value = false
  error.value = ''
  try {
    applySecurityDraft()
    const payload = await api.saveSettings(settings.value)
    settings.value = payload.settings
    syncBlockedUserAgents(settings.value)
    securitySaved.value = true
    emit('saved')
  } catch (saveError) {
    error.value = saveError.message
  } finally {
    securityBusy.value = false
  }
}

async function downloadBackup() {
  backupBusy.value = true
  error.value = ''
  try {
    const blob = await api.backupSettings()
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = 'aetherlink-config.yaml'
    anchor.click()
    URL.revokeObjectURL(url)
  } catch (backupError) {
    error.value = backupError.message
  } finally {
    backupBusy.value = false
  }
}

function selectRestoreFile() {
  restoreInput.value?.click()
}

async function restoreBackup(event) {
  const file = event.target.files?.[0]
  event.target.value = ''
  if (!file) return
  if (!window.confirm(`确认还原配置文件“${file.name}”？当前配置会被覆盖。`)) return
  backupBusy.value = true
  error.value = ''
  try {
    const content = await file.text()
    await api.restoreSettings(content)
    saved.value = true
    await load()
    emit('saved')
  } catch (restoreError) {
    error.value = restoreError.message
  } finally {
    backupBusy.value = false
  }
}

function saveAccount() {
  accountError.value = ''
  if (!username.value.trim() || !password.value) {
    accountError.value = '账号和密码不能为空'
    return
  }
  accountConfirm.value = true
}

async function confirmAccountSave() {
  accountConfirm.value = false
  accountBusy.value = true
  try {
    await api.updateAccount(username.value, password.value)
    password.value = ''
    emit('account-changed')
  } catch (saveError) {
    accountError.value = saveError.message
  } finally {
    accountBusy.value = false
  }
}

onMounted(load)
onUnmounted(() => {
  cardResizeObserver?.disconnect()
  cardResizeObserver = null
})
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
        <section ref="accountCard" class="settings-card account-card">
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
              <h2>登录账号</h2>
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
              <span>密码</span>
              <input v-model="password" type="password" autocomplete="new-password" placeholder="输入新密码" />
            </label>
          </div>

          <button class="primary settings-save-button wide-action" :disabled="accountBusy" @click="saveAccount">
            {{ accountBusy ? '保存中…' : '保存修改' }}
          </button>
          <p v-if="accountError" class="error form-error">{{ accountError }}</p>
          <p class="card-footnote">保存后需要重新登录。</p>
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

        <section class="settings-card backup-card">
          <div class="settings-card-head compact">
            <div class="settings-icon blue" aria-hidden="true">
              <svg viewBox="0 0 24 24"><path d="M4 7h16v13H4z" /><path d="M8 7V4h8v3M8 12h8M8 16h5" /></svg>
            </div>
            <div>
              <h2>备份配置</h2>
              <p>保存或还原 AetherLink 配置</p>
            </div>
          </div>
          <div class="backup-actions">
            <button class="secondary settings-save-button" :disabled="backupBusy" @click="downloadBackup">备份</button>
            <button class="secondary settings-save-button" :disabled="backupBusy" @click="selectRestoreFile">还原</button>
            <input ref="restoreInput" class="visually-hidden" type="file" accept=".yaml,.yml,text/yaml" @change="restoreBackup" />
          </div>
        </section>
      </aside>

      <div class="settings-main">
        <section ref="systemCard" class="settings-card system-card">
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
            <div class="security-head-actions">
              <span v-if="saved" class="save-confirm"><i></i>已保存</span>
              <button class="primary settings-save-button compact-save-button" :disabled="busy" @click="save">
                {{ busy ? '保存中…' : '保存' }}
              </button>
            </div>
          </div>

          <div class="settings-section">
            <div class="settings-section-title">
              <span>缓存与日志</span>
              <small>重复播放更快，保留必要的排障记录</small>
            </div>
            <div class="form-grid two">
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

        </section>

        <section class="settings-card security-card">
          <div class="settings-card-head security-head">
            <div class="settings-icon violet" aria-hidden="true">
              <svg viewBox="0 0 24 24">
                <path d="M12 3 5 6v5c0 4.6 2.9 8.3 7 10 4.1-1.7 7-5.4 7-10V6z" />
                <path d="m9 12 2 2 4-4" />
              </svg>
            </div>
            <div>
              <h2>安全与代理</h2>
              <p>控制客户端 User-Agent 的转发</p>
            </div>
            <div class="security-head-actions">
              <span v-if="securitySaved" class="save-confirm"><i></i>已保存</span>
              <button class="primary settings-save-button compact-save-button" :disabled="securityBusy" @click="saveSecurity">
                {{ securityBusy ? '保存中…' : '保存' }}
              </button>
            </div>
          </div>

          <div class="ua-block-grid">
            <div class="ua-policy-panel">
              <label class="setting-toggle security-toggle">
                <input type="checkbox" v-model="settings.redirect.blockClientUserAgentEmby" />
                <span class="toggle-control"></span>
                <span class="toggle-copy"><strong>Emby 屏蔽 UA</strong><small>仅作用于下面选中的 Emby 服务</small></span>
              </label>
              <label class="field security-field">
                <span>匹配片段</span>
                <textarea v-model="blockedEmbyUserAgentText" rows="4" :disabled="!settings.redirect.blockClientUserAgentEmby" placeholder="/Infuse/\nForward\nEmby Theater"></textarea>
              </label>
              <div class="candidate-box">
                <span class="candidate-title">候选服务器</span>
                <details class="candidate-picker" :class="{ disabled: !settings.redirect.blockClientUserAgentEmby }">
                  <summary @click="preventCandidateMenu($event, settings.redirect.blockClientUserAgentEmby)"><span class="candidate-summary-text">{{ candidateSummary('emby') }}</span><b>{{ selectedUpstreams('emby').length }}</b></summary>
                  <div class="candidate-menu">
                    <button
                      v-for="upstream in candidateUpstreams('emby')"
                      :key="upstream.name"
                      type="button"
                      class="candidate-option"
                      :class="{ selected: isCandidateSelected('emby', upstream.name) }"
                      :disabled="!settings.redirect.blockClientUserAgentEmby"
                      @click="toggleCandidate('emby', upstream.name)"
                    >
                      <span>{{ upstream.name }}</span><i>{{ isCandidateSelected('emby', upstream.name) ? '已选' : '选择' }}</i>
                    </button>
                    <small v-if="!candidateUpstreams('emby').length" class="candidate-empty">暂无 Emby 服务</small>
                  </div>
                </details>
              </div>
            </div>
            <div class="ua-policy-panel">
              <label class="setting-toggle security-toggle">
                <input type="checkbox" v-model="settings.redirect.blockClientUserAgentAudiobookshelf" />
                <span class="toggle-control"></span>
                <span class="toggle-copy"><strong>AudioBookShelf 屏蔽 UA</strong><small>仅作用于下面选中的 ABS 服务</small></span>
              </label>
              <label class="field security-field">
                <span>匹配片段</span>
                <textarea v-model="blockedAudiobookshelfUserAgentText" rows="4" :disabled="!settings.redirect.blockClientUserAgentAudiobookshelf" placeholder="/Komic-iOS/\nListenAudiobook"></textarea>
              </label>
              <div class="candidate-box">
                <span class="candidate-title">候选服务器</span>
                <details class="candidate-picker" :class="{ disabled: !settings.redirect.blockClientUserAgentAudiobookshelf }">
                  <summary @click="preventCandidateMenu($event, settings.redirect.blockClientUserAgentAudiobookshelf)"><span class="candidate-summary-text">{{ candidateSummary('audiobookshelf') }}</span><b>{{ selectedUpstreams('audiobookshelf').length }}</b></summary>
                  <div class="candidate-menu">
                    <button
                      v-for="upstream in candidateUpstreams('audiobookshelf')"
                      :key="upstream.name"
                      type="button"
                      class="candidate-option"
                      :class="{ selected: isCandidateSelected('audiobookshelf', upstream.name) }"
                      :disabled="!settings.redirect.blockClientUserAgentAudiobookshelf"
                      @click="toggleCandidate('audiobookshelf', upstream.name)"
                    >
                      <span>{{ upstream.name }}</span><i>{{ isCandidateSelected('audiobookshelf', upstream.name) ? '已选' : '选择' }}</i>
                    </button>
                    <small v-if="!candidateUpstreams('audiobookshelf').length" class="candidate-empty">暂无 AudioBookShelf 服务</small>
                  </div>
                </details>
              </div>
            </div>
          </div>
        </section>
      </div>
    </div>

    <div v-if="accountConfirm" class="modal-backdrop" @click.self="accountConfirm = false">
      <div class="modal account-confirm-modal">
        <div class="modal-head">
          <h2>确认修改账号</h2>
          <button class="ghost close" @click="accountConfirm = false">关闭</button>
        </div>
        <div class="modal-body">
          <p>保存后当前登录会话会失效，需要使用新账号和密码重新登录。</p>
        </div>
        <div class="modal-foot">
          <div class="right">
            <button @click="accountConfirm = false">取消</button>
            <button class="primary" :disabled="accountBusy" @click="confirmAccountSave">确认修改</button>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>
