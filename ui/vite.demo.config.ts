import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'

const root = dirname(fileURLToPath(import.meta.url))
const replacements = new Map(['api', 'live', 'pwa'].map((name) => [resolve(root, `src/lib/${name}.ts`), resolve(root, `src/demo/${name}.ts`)]))

export function isolatedDemo(): Plugin {
  return { name: 'isolated-demo', enforce: 'pre',
    resolveId(source, importer) {
      if (!importer || (!source.startsWith('.') && !source.startsWith('/'))) return null
      const candidate = resolve(dirname(importer.split('?')[0]), source.split('?')[0])
      return replacements.get(candidate) ?? replacements.get(`${candidate}.ts`) ?? null
    },
    transformIndexHtml(html) {
      return html.replace('<head>', `<head><meta http-equiv="Content-Security-Policy" content="default-src 'self'; connect-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'">`)
    },
    generateBundle(_options, bundle) {
      for (const item of Object.values(bundle)) {
        if (item.type === 'chunk' && Object.keys(item.modules).some((id) => replacements.has(id.split('?')[0]))) {
          this.error('The isolated demo must not contain the production API, live-channel, or service-worker registration implementation.')
        }
      }
    },
  }
}

export default defineConfig({ plugins: [isolatedDemo(), react()], base: './',
  build: { outDir: 'demo-dist', chunkSizeWarningLimit: 700 },
  // Intentionally no daemon proxy: this is a separate, self-contained build.
  server: { host: '127.0.0.1', port: 4180, strictPort: true }, preview: { host: '127.0.0.1', port: 4180, strictPort: true },
})
