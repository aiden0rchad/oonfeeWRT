import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Presentation-only browser tests must never inherit the development proxy to
// a controller. Every API response is supplied explicitly by the test.
export default defineConfig({
  plugins: [react()],
  base: './',
})
