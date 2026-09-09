// 莫奈低饱和动态色板。
//
// 取色来自莫奈的睡莲与干草堆：晨蓝、水草绿、藕紫、淡金、鼠尾草。饱和度压得很
// 低，长时间盯着不刺眼；每张卡片是三段渐变而不是两段，配合 CSS 里的缓慢位移
// 动画，颜色会像水面一样慢慢流动。
//
// 卡片配色每次加载随机分配，避免所有服务长期使用相同色调；动画起始相位也随
// 当前页面随机变化，让卡片之间不会整齐地一起动。
const GRADIENTS = [
  ['#8fa3c4', '#a4b8c8', '#c3b3c9'],
  ['#9db8a8', '#adc0ac', '#c8c49d'],
  ['#c1abbd', '#b3a6c1', '#a9bcc9'],
  ['#c9bb98', '#bcb69c', '#a7b9b0'],
  ['#a3aec9', '#b6b4c6', '#c6bbb2'],
  ['#a8bfb4', '#bcc8b2', '#cdc3aa'],
  ['#c6b0ab', '#c0adbd', '#a8b3c7'],
  ['#93aebd', '#a6bcc0', '#bcc6ae']
]

// 动画周期，和 styles.css 里的 --card-tide-duration 保持一致。
const TIDE_SECONDS = 26
const cardPalettes = new Map()
const cardDelays = new Map()

export function gradientFor(name) {
  const key = String(name || '')
  if (!cardPalettes.has(key)) {
    cardPalettes.set(key, Math.floor(Math.random() * GRADIENTS.length))
  }
  const [from, via, to] = GRADIENTS[cardPalettes.get(key)]
  return `linear-gradient(145deg, ${from} 0%, ${via} 45%, ${to} 100%)`
}

// 负延迟让动画一进页面就停在随机相位，不会出现所有卡片同时从头开始。
export function tideDelayFor(name) {
  const key = String(name || '')
  if (!cardDelays.has(key)) {
    cardDelays.set(key, Math.floor(Math.random() * TIDE_SECONDS))
  }
  return `-${cardDelays.get(key)}s`
}

// 卡片要的全部内联样式：渐变 + 动画相位。
export function cardStyleFor(name) {
  return { backgroundImage: gradientFor(name), animationDelay: tideDelayFor(name) }
}
