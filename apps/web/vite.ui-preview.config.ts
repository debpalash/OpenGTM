import { fileURLToPath } from "node:url"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from "vite"
import { twentyCss } from "./config/twenty-css"

export default defineConfig({
  base: "/ui-preview/",
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  plugins: [react(), tailwindcss()],
  css: { postcss: { plugins: [twentyCss()] } },
  build: {
    outDir: "dist/ui-preview",
    rollupOptions: {
      input: fileURLToPath(new URL("./ui-preview.html", import.meta.url))
    }
  }
})
