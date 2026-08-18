import { defineConfig } from 'vite';

/**
 * Development server proxies the API and tiles to the Go backend, so the
 * frontend runs on the same origin it will in production and no CORS
 * configuration is needed during development.
 */
export default defineConfig({
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
      '/tiles': { target: 'http://localhost:8080', changeOrigin: true },
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
    // OpenLayers is large; splitting it out keeps app rebuilds off the
    // critical path for returning visitors.
    rollupOptions: {
      output: {
        manualChunks: { ol: ['ol'] },
      },
    },
  },
});
