<script setup>
import { onMounted, onUnmounted, ref, watch } from 'vue'

const props = defineProps({
  x: { type: Number, required: true },
  y: { type: Number, required: true },
  title: { type: String, default: '' }
})
const emit = defineEmits(['close'])

const element = ref(null)
const position = ref({ left: props.x, top: props.y })

// 菜单在靠近视口右/下边缘时会被裁掉，挂载后按实际尺寸回折。
function clamp() {
  const node = element.value
  if (!node) return
  const { width, height } = node.getBoundingClientRect()
  const margin = 8
  position.value = {
    left: Math.min(props.x, window.innerWidth - width - margin),
    top: Math.min(props.y, window.innerHeight - height - margin)
  }
}

function onKeydown(event) {
  if (event.key === 'Escape') emit('close')
}

onMounted(() => {
  clamp()
  // 捕获阶段监听：点菜单项时先触发按钮自己的 click，再由它决定是否关闭。
  window.addEventListener('keydown', onKeydown)
  window.addEventListener('resize', clamp)
})

onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
  window.removeEventListener('resize', clamp)
})

watch(() => [props.x, props.y], clamp)
</script>

<template>
  <!-- 透明遮罩兜住任意点击与再次右键，保证菜单一定能关掉。 -->
  <div
    class="menu-scrim"
    @click="emit('close')"
    @contextmenu.prevent="emit('close')"
    @wheel="emit('close')"
  >
    <div
      ref="element"
      class="context-menu"
      :style="{ left: `${position.left}px`, top: `${position.top}px` }"
      @click.stop
      @contextmenu.prevent.stop
    >
      <div v-if="title" class="label">{{ title }}</div>
      <slot />
    </div>
  </div>
</template>

<style scoped>
.menu-scrim {
  position: fixed;
  inset: 0;
  z-index: 55;
}
</style>
