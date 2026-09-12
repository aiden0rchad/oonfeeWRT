<script setup lang="ts">
import { computed } from 'vue'
import { withBase } from 'vitepress'

const props = defineProps<{
  src: string
  alt: string
  caption: string
  width?: number
  height?: number
}>()
const source = computed(() => withBase(`/screenshots/${props.src}-dark.jpg`))
</script>

<template>
  <figure class="docs-screenshot" :data-portrait="(width ?? 1620) < (height ?? 959)">
    <a :href="source" target="_blank" rel="noopener" :aria-label="`Open full-size screenshot: ${alt}`">
      <img :src="source" :alt="alt" :width="width ?? 1620" :height="height ?? 959" loading="lazy" decoding="async">
    </a>
    <figcaption>
      {{ caption }}
      <span class="docs-screenshot-meta">Dark-mode capture · Select image to open full size</span>
    </figcaption>
  </figure>
</template>
