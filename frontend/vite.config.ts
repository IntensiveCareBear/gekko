import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // Honor a harness-assigned port (e.g. preview tooling) when provided.
    port: process.env.PORT ? Number(process.env.PORT) : undefined,
    proxy: {
      '/api': {
        // LIVEVIEW_PROXY lets a dev instance point at a non-default liveview
        // (e.g. one on a side port) without touching a running stack.
        target: process.env.LIVEVIEW_PROXY || 'http://localhost:8082',
        changeOrigin: true,
      }
    }
  }
})


