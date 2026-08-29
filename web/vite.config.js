import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The built SPA is embedded into the Go binary from ../internal/web/dist and is
// served under /aetherlink/, so asset URLs must use that base.
export default defineConfig({
  plugins: [vue()],
  base: '/aetherlink/',
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true
  },
  server: {
    port: 5173,
    proxy: {
      // During development the Vite server forwards API calls to a local
      // AetherLink instance so the UI can run with hot reload.
      '/aetherlink/api': {
        target: 'http://127.0.0.1:5151',
        changeOrigin: true
      }
    }
  }
})
