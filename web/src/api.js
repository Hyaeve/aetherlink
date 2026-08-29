// AetherLink 管理接口的轻量客户端。
//
// 会话令牌存在 localStorage 里，通过 Authorization 头发送，不放进查询串，
// 免得出现在反代的访问日志里。令牌由口令登录换取，服务重启后失效。
const BASE = '/aetherlink/api'
const TOKEN_KEY = 'aetherlink.session'

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
    // code 让界面区分「需要初始化」和「需要登录」两种失败。
    error.code = payload?.code || ''
    throw error
  }
  return payload
}

const jsonBody = (payload) => ({ body: JSON.stringify(payload ?? {}) })

export const api = {
  setupState: () => request('/setup/state'),
  setup: (password) => request('/setup', { method: 'POST', ...jsonBody({ password }) }),
  login: (password) => request('/login', { method: 'POST', ...jsonBody({ password }) }),
  logout: () => request('/logout', { method: 'POST', ...jsonBody({}) }),
  changePassword: (currentPassword, newPassword) =>
    request('/password', { method: 'POST', ...jsonBody({ currentPassword, newPassword }) }),

  status: () => request('/status'),
  config: () => request('/config'),
  saveSettings: (settings) => request('/settings', { method: 'PUT', ...jsonBody(settings) }),

  upstreams: () => request('/upstreams'),
  createUpstream: (payload) => request('/upstreams', { method: 'POST', ...jsonBody(payload) }),
  updateUpstream: (name, payload) =>
    request(`/upstreams/${encodeURIComponent(name)}`, { method: 'PUT', ...jsonBody(payload) }),
  deleteUpstream: (name) => request(`/upstreams/${encodeURIComponent(name)}`, { method: 'DELETE' }),
  testUpstream: (payload) => request('/upstreams/test', { method: 'POST', ...jsonBody(payload) }),

  ping: (name) => request(`/upstreams/${encodeURIComponent(name)}/ping`),
  libraries: (name) => request(`/upstreams/${encodeURIComponent(name)}/libraries`),
  items: (name, params) => request(`/upstreams/${encodeURIComponent(name)}/items?${new URLSearchParams(params)}`),
  itemFiles: (name, itemId) =>
    request(`/upstreams/${encodeURIComponent(name)}/items/${encodeURIComponent(itemId)}`),
  resolve: (name, params) => request(`/upstreams/${encodeURIComponent(name)}/resolve?${new URLSearchParams(params)}`),
  parseStrm: (payload) => request('/strm/parse', { method: 'POST', ...jsonBody(payload) }),
  stats: (events = 50) => request(`/stats?events=${events}`),
  logs: (limit = 200) => request(`/logs?limit=${limit}`),
  purgeCache: () => request('/cache/purge', { method: 'POST' })
}
