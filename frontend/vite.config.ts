import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Backend dev server is expected on :8080 (see backend/internal/config);
// proxying /api keeps the frontend dev server same-origin-equivalent so
// cookies (admin session + CSRF) behave the same as in production, where
// the Go backend serves the built frontend directly.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
  },
})
