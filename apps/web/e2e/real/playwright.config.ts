import { defineConfig } from "@playwright/test"

// Real-backend journey. Not part of `test:a11y` (that config only looks in ./e2e/a11y) and not run in CI.
// Point it at a running stack with OPENGTM_E2E_BASE_URL (the nginx port). See README.md.
const baseURL = process.env.OPENGTM_E2E_BASE_URL

export default defineConfig({
  testDir: ".",
  testMatch: "*.spec.ts",
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 20 * 60_000,
  reporter: [["list"]],
  outputDir: process.env.OPENGTM_E2E_OUT ? `${process.env.OPENGTM_E2E_OUT}/test-results` : "test-results-real",
  use: {
    baseURL,
    browserName: "chromium",
    viewport: { width: 1440, height: 1000 },
    locale: "en-US",
    timezoneId: "UTC",
    // Animations off so screenshots are stable; everything else is the real app.
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    reducedMotion: "reduce",
    serviceWorkers: "block"
  }
})
