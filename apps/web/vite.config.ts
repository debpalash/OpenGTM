import { fileURLToPath } from "node:url"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"
import { twentyCss } from "./config/twenty-css"

// Dev proxy target: nginx (docker compose, port 3000) so `make dev` routes like the deployed stack.
// Point VITE_API_TARGET at a bare Go API (e.g. http://127.0.0.1:8000) to skip nginx.
const apiTarget = process.env.VITE_API_TARGET || "http://127.0.0.1:3000"

export default defineConfig({
  build: { manifest: true },
  plugins: [react(), tailwindcss()],
  css: { postcss: { plugins: [twentyCss()] } },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  server: {
    port: 4099,
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        ws: true,
      },
      "/auth": {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
})
