// AetherLink 管理接口的轻量客户端。
//
// 会话令牌存在 localStorage 里，通过 Authorization 头发送，不放进查询串，
// 免得出现在反代的访问日志里。令牌由口令登录换取，服务重启后失效。
const BASE = '/aetherlink/api'
const TOKEN_KEY = 'aetherlink.session'

// 会话失效（容器重启、令牌到期、账号改过）时的回调，由 App 注册：清掉界面状态、
// 退回登录页。判断集中在这里——否则每个页面都要各判一次，还容易把后端那句
// 「会话无效或已过期，请重新登录」当成普通错误挂在界面上。
let sessionExpiredHandler = null

export function setSessionExpiredHandler(handler) {
  sessionExpiredHandler = handler
}

// visibleMessage 给出适合直接显示的错误文案：会话失效返回空串，调用方的
// `v-if="error"` 自然就不渲染了——那一刻界面已经回登录页，再挂一条错误只会让人
// 以为出了别的问题。其它错误原样返回。
export function visibleMessage(error) {
  if (error && error.sessionExpired) return ''
  return (error && error.message) || ''
}

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setToken(token) {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token)
  } else {
    localStorage.removeItem(TOKEN_KEY)
  }
}

async function request(path, options = {}) {
  const headers = { Accept: 'application/json', ...(options.headers || {}) }
  const token = getToken()
  if (token) {
    headers.Authorization = `Bearer ${token}`
  }
  if (options.body) {
    headers['Content-Type'] = 'application/json'
  }

  const response = await fetch(`${BASE}${path}`, { ...options, headers })
  const text = await response.text()
  let payload = null
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = { error: text }
    }
  }
  if (!response.ok) {
    const message = payload?.error || `${response.status} ${response.statusText}`
    const error = new Error(message)
    error.status = response.status
    // code 让界面区分不同的失败原因（目前只有 unauthorized）。
    error.code = payload?.code || ''
    // 带着令牌还被回 401，就是会话失效：清掉本地令牌并让界面直接回登录页，而不是
    // 把后端的 401 文案当错误显示出来。（不带令牌的 401 是登录口令错误，交给登录
    // 表单自己处理，不走这条路。）
    if (response.status === 401 && token) {
      error.sessionExpired = true
      setToken('')
      if (sessionExpiredHandler) sessionExpiredHandler()
    }
    throw error
  }
  return payload
}

const jsonBody = (payload) => ({ body: JSON.stringify(payload ?? {}) })

export const api = {
  bootstrap: () => request('/bootstrap'),
  // remember 来自登录页的「保持登录」：勾上之后后端签发的会话管 7 天（固定不顺延），
  // 但**只对当前这个浏览器、且容器没重启过**有效 —— 容器一重启所有会话一起失效。
  // 不勾就还是 12 小时、随用顺延的普通会话。
  login: (username, password, remember = false) =>
    request('/login', { method: 'POST', ...jsonBody({ username, password, remember: !!remember }) }),
  logout: () => request('/logout', { method: 'POST', ...jsonBody({}) }),
  updateAccount: (username, password) =>
    request('/account', { method: 'POST', ...jsonBody({ username, password }) }),

  status: () => request('/status'),
  config: () => request('/config'),
  saveSettings: (settings) => request('/settings', { method: 'PUT', ...jsonBody(settings) }),
  backupSettings: async () => {
    const response = await fetch(`${BASE}/settings/backup`, {
      headers: { Authorization: `Bearer ${getToken()}` }
    })
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`)
    return response.blob()
  },
  restoreSettings: (content) => request('/settings/restore', { method: 'POST', ...jsonBody({ content }) }),

  upstreams: () => request('/upstreams'),
  createUpstream: (payload) => request('/upstreams', { method: 'POST', ...jsonBody(payload) }),
  updateUpstream: (name, payload) =>
    request(`/upstreams/${encodeURIComponent(name)}`, { method: 'PUT', ...jsonBody(payload) }),
  deleteUpstream: (name) => request(`/upstreams/${encodeURIComponent(name)}`, { method: 'DELETE' }),
  testUpstream: (payload) => request('/upstreams/test', { method: 'POST', ...jsonBody(payload) }),

  ping: (name) => request(`/upstreams/${encodeURIComponent(name)}/ping`),
  // 列表接口只给 hasApiKey / hasPassword，秘密的值要靠这一条单独取。
  // 编辑窗口打开时调一次，把值填进字段并保持遮蔽（圆点），
  // 用户点输入框右侧的小眼睛就能看明文，不必再走第二次请求。
  upstreamCredentials: (name) => request(`/upstreams/${encodeURIComponent(name)}/credentials`),
  logs: (limit = 5000) => request(`/logs?limit=${limit}`),
  // stats 是播放事件流水：每条媒体请求最终是 302、透传还是中继都在这里。
  stats: (events = 5000) => request(`/stats?events=${events}`)
}
