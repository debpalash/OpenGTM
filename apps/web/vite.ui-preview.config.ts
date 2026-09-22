import { fileURLToPath } from "node:url"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"
import { twentyCss } from "./config/twenty-css"

export default defineConfig({
  plugins: [react()],
  css: { postcss: { plugins: [twentyCss()] } },
  build: {
    outDir: "dist/ui-preview",
    rollupOptions: {
      input: fileURLToPath(new URL("./ui-preview.html", import.meta.url)),
    },
  },
})
