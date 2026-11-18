import { defineConfig } from "@playwright/test"

const PORT = 4173

export default defineConfig({
  testDir: "./e2e/a11y",
  testMatch: "*.spec.ts",
  fullyParallel: true,
  // Baseline regeneration merges into one JSON file, so it must run serially.
  workers: process.env.A11Y_UPDATE_BASELINE ? 1 : process.env.CI ? 2 : 4,
  retries: 0,
  reporter: [["list"], ["html", { open: "never", outputFolder: "playwright-report" }]],
  outputDir: "test-results",
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    browserName: "chromium",
    reducedMotion: "reduce",
    locale: "en-US",
    timezoneId: "UTC",
    serviceWorkers: "block"
  },
  webServer: {
    // Serves the already-built dist (run `bun run build` first).
    command: `bunx vite preview --host 127.0.0.1 --port ${PORT} --strictPort`,
    url: `http://127.0.0.1:${PORT}`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000
  }
})
