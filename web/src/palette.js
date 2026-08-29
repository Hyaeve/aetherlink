// 莫奈低饱和动态色板。
//
// 取色来自莫奈的睡莲与干草堆：晨蓝、水草绿、藕紫、淡金、鼠尾草。饱和度压得很
// 低，长时间盯着不刺眼；每张卡片是三段渐变而不是两段，配合 CSS 里的缓慢位移
// 动画，颜色会像水面一样慢慢流动。
//
// 卡片配色由名称哈希决定而不是随机数：这样同一个上游每次刷新、每台设备上都是
// 同一个颜色，用户可以靠颜色记住卡片，而不是每次进页面都换一遍。动画的起始相
// 位也由哈希决定，卡片之间就不会整齐地一起动。
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

function hash(value) {
  let result = 0
  for (let index = 0; index < value.length; index += 1) {
    result = (result * 31 + value.charCodeAt(index)) >>> 0
  }
  return result
}

export function gradientFor(name) {
  const [from, via, to] = GRADIENTS[hash(String(name || '')) % GRADIENTS.length]
  return `linear-gradient(145deg, ${from} 0%, ${via} 45%, ${to} 100%)`
}

// 负延迟让动画一进页面就停在相位中间，不会出现「所有卡片同时从头开始」。
export function tideDelayFor(name) {
  return `-${hash(String(name || '')) % TIDE_SECONDS}s`
}

// 卡片要的全部内联样式：渐变 + 动画相位。
export function cardStyleFor(name) {
  return { backgroundImage: gradientFor(name), animationDelay: tideDelayFor(name) }
}