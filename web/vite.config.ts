import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import ui from '@nuxt/ui/vite'

// The Go server of the development instance (contrib/dev/seed.sh). Its
// config allows the origin http://localhost:5173, so the proxy passes the
// browser's Origin and Host on unchanged.
const api = process.env.ROBINMAIL_DEV_URL ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [
    vue(),
    ui({
      // Icons are bundled at build time from @iconify-json/lucide: the
      // page's CSP allows no request to an icon API. The scan reads the
      // names ("i-lucide-...") from the sources, also from .ts files,
      // which it leaves out by default.
      icon: { clientBundle: { scan: { globInclude: ['src/**/*.{vue,ts}'], globExclude: ['src/**/*.test.ts', 'src/test/**'] } } },
      ui: { colors: { primary: 'orange', neutral: 'stone' } },
    }),
  ],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/jmap': api,
      '/.well-known/jmap': api,
      '/auth': api,
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // No inline scripts or module preload polyfill: the CSP has script-src 'self'.
    modulePreload: { polyfill: false },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
