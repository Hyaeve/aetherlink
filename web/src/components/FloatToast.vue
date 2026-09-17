<script setup>
// 页面级悬浮提示：圆角、毛玻璃、清透，亮完自己隐去。
//
// 与页面顶部那种横栏提示的区别是它不占布局——卡片、工具栏都不会被顶下去，
// 适合「点了某个操作、只想知道结果」的反馈（比如右键菜单里的测试）。
// 计时器归组件自己管：文案一变就重新计时，所以「正在测试…」换成结果之后，
// 停留时间按结果这条重新算。loading 态不自动消失——请求还没回来，
// 提示得一直挂着，否则用户会以为点空了。
import { onUnmounted, watch } from 'vue'

const props = defineProps({
  text: { type: String, required: true },
  // info 中性、ok 成功、danger 失败；只用来染图标与描边，正文始终是常规字色。
  tone: { type: String, default: 'info' },
  loading: { type: Boolean, default: false },
  // 停留时长（毫秒）。失败信息通常更长，调用方可以给久一点。
  duration: { type: Number, default: 4000 }
})
const emit = defineEmits(['done'])

let timer = 0

function schedule() {
  window.clearTimeout(timer)
  if (props.loading) return
  timer = window.setTimeout(() => emit('done'), Math.max(0, props.duration))
}

watch(() => [props.text, props.loading, props.duration], schedule, { immediate: true })

onUnmounted(() => window.clearTimeout(timer))
</script>

<template>
  <div class="float-toast" :class="[tone, { loading }]" role="status" aria-live="polite">
    <span class="float-toast-indicator" aria-hidden="true">
      <span v-if="loading" class="spinner"></span>
      <svg v-else-if="tone === 'ok'" viewBox="0 0 24 24"><path d="m5 13 4.5 4.5L19 7" /></svg>
      <svg v-else-if="tone === 'danger'" viewBox="0 0 24 24"><path d="M6.5 6.5l11 11M17.5 6.5l-11 11" /></svg>
      <span v-else class="dot"></span>
    </span>
    <span class="float-toast-text">{{ text }}</span>
  </div>
</template>
