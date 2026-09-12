import DefaultTheme from 'vitepress/theme-without-fonts'
import type { Theme } from 'vitepress'
import DocScreenshot from './DocScreenshot.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('DocScreenshot', DocScreenshot)
  },
} satisfies Theme
