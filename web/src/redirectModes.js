// 跳转模式的四档定义，卡片右上角的下拉与编辑弹窗里的「跳转模式」共用这一份。
//
// 每档带一组 24×24 的描边路径（`stroke: currentColor`、`fill: none`，尺寸由宿主给），
// 语义按「流量往哪走」来分：
//   always  方框 + 斜向外跳的箭头——不在本地停留，直接 302 到直链
//   public  地球——按客户端是不是从公网来分流
//   private 房子——按客户端是不是从内网来分流
//   never   折返箭头——不直跳，绕经 AetherLink 代取后再回给客户端
export const REDIRECT_OPTIONS = [
  {
    value: 'always',
    label: '始终跳转',
    paths: [
      'M13.6 5H6.8A2.3 2.3 0 0 0 4.5 7.3v9.9A2.3 2.3 0 0 0 6.8 19.5h9.9a2.3 2.3 0 0 0 2.3-2.3v-6.8',
      'M14.6 4.5h4.9v4.9',
      'M19.2 4.8 12.5 11.5'
    ]
  },
  {
    value: 'public',
    label: '公网跳转',
    paths: [
      'M12 4.4a7.6 7.6 0 1 0 0 15.2 7.6 7.6 0 1 0 0-15.2z',
      'M4.6 12h14.8',
      'M12 4.4c-2.3 2.3-3.4 4.8-3.4 7.6s1.1 5.3 3.4 7.6',
      'M12 4.4c2.3 2.3 3.4 4.8 3.4 7.6s-1.1 5.3-3.4 7.6'
    ]
  },
  {
    value: 'private',
    label: '内网跳转',
    paths: [
      'M3.9 10.7 12 4.3l8.1 6.4',
      'M6.3 10.4v7.9A1.5 1.5 0 0 0 7.8 19.8h8.4a1.5 1.5 0 0 0 1.5-1.5v-7.9',
      'M10.3 19.8v-4.5h3.4v4.5'
    ]
  },
  {
    value: 'never',
    label: '始终中继',
    paths: [
      'M3.9 4.4h10.5a5.7 5.7 0 0 1 0 11.4H7',
      'M10.7 12 7 15.8l3.7 3.8'
    ]
  }
]

export const REDIRECT_LABELS = Object.fromEntries(REDIRECT_OPTIONS.map((option) => [option.value, option.label]))

// 拿不到取值时退回第一档（始终跳转），与卡片上原来的兜底一致。
export function redirectLabel(mode) {
  return REDIRECT_LABELS[mode] || REDIRECT_LABELS.always
}

export function redirectPaths(mode) {
  return (REDIRECT_OPTIONS.find((option) => option.value === mode) || REDIRECT_OPTIONS[0]).paths
}

export function redirectUnstable(type, mode) {
  return type === 'fnos' && mode !== 'always'
}
