import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In dev, vite serves the SPA and proxies /api to the Go backend, so both
// hot-reload independently. In prod, the Go binary embeds dist/.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true, sourcemap: false },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:7070', ws: true },
    },
  },
})
