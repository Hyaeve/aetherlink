// 莫兰迪低饱和渐变色板。
//
// 卡片配色由名称哈希决定而不是随机数：这样同一个上游每次刷新、每台设备上
// 都是同一个颜色，用户可以靠颜色记住卡片，而不是每次进页面都换一遍。
const GRADIENTS = [
  ['#a8b8a4', '#8f9f95'],
  ['#a9bcc6', '#94a7b4'],
  ['#c3b3bd', '#a99aa6'],
  ['#c8bda9', '#b0a48f'],
  ['#adb6c4', '#9aa2b2'],
  ['#bfc4ac', '#a6ac94'],
  ['#c9b7ae', '#b09c94'],
  ['#a6bdb6', '#8fa8a1']
]

function hash(value) {
  let result = 0
  for (let index = 0; index < value.length; index += 1) {
    result = (result * 31 + value.charCodeAt(index)) >>> 0
  }
  return result
}

export function gradientFor(name) {
  const [from, to] = GRADIENTS[hash(String(name || '')) % GRADIENTS.length]
  return `linear-gradient(150deg, ${from}, ${to})`
}
