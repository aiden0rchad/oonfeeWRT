import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The build output is embedded in the Go binary (ui/dist -> go:embed), so the
// base is relative and nothing may reference an absolute host path.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist',
    // The bundle budget is 1.5 MB gzipped (DEVICE-BUDGET §8). Warn well before
    // that so the ceiling is a decision rather than a surprise.
    chunkSizeWarningLimit: 700,
  },
  server: {
    // `npm run dev` talks to a locally running daemon.
    proxy: { '/api': 'http://127.0.0.1:8080' },
  },
  test: {
    // happy-dom rather than jsdom: same job, materially smaller dependency
    // tree, and this repo is public — every dev dependency is surface.
    environment: 'happy-dom',
    globals: true,
    setupFiles: ['./src/test-setup.ts'],
    // Only real test files. `*.check.ts` is the no-runner stopgap that predates
    // this and runs under plain node; picking it up here would run its
    // assertions twice and report zero tests for the file.
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
