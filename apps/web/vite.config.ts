import { fileURLToPath } from "node:url"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"
import { twentyCss } from "./config/twenty-css.ts"

// Dev proxy target: nginx (docker compose, port 3000) so `make dev` routes like the deployed stack.
// Point VITE_API_TARGET at a bare Go API (e.g. http://127.0.0.1:8000) to skip nginx.
const apiTarget = process.env.VITE_API_TARGET || "http://127.0.0.1:3000"

export default defineConfig({
  build: {
    manifest: true,
    rolldownOptions: {
      input: {
        app: fileURLToPath(new URL("./index.html", import.meta.url)),
        "ui-preview": fileURLToPath(
          new URL("./ui-preview.html", import.meta.url)
        )
      },
      output: {
        // Long-lived vendor chunks: cached across app releases and each well under the 500 kB warning.
        codeSplitting: {
          groups: [
            {
              name: "vendor-react",
              test: /node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom)[\\/]/,
              priority: 30
            },
            {
              name: "vendor-ui",
              test: /node_modules[\\/](@base-ui|@floating-ui|lucide-react|sonner|cmdk|class-variance-authority|tailwind-merge|clsx)[\\/]/,
              priority: 20
            },
            {
              name: "vendor-data",
              test: /node_modules[\\/]@tanstack[\\/]/,
              priority: 20
            }
          ]
        }
      }
    }
  },
  plugins: [react(), tailwindcss()],
  css: { postcss: { plugins: [twentyCss()] } },
  resolve: {
    dedupe: ["react", "react-dom"],
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url))
    }
  },
  server: {
    fs: {
      allow: [
        fileURLToPath(new URL("./", import.meta.url)),
        fileURLToPath(new URL("../../node_modules", import.meta.url))
      ]
    },
    port: 4099,
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        ws: true
      },
      "/auth": {
        target: apiTarget,
        changeOrigin: true
      }
    }
  }
})
