import { describe, expect, test } from "bun:test"
import { readdirSync, readFileSync, statSync } from "node:fs"
import { join } from "node:path"
import { nextStepFor, rememberNextStep, safeInternalPath } from "../src/lib/notify"

describe("nextStepFor", () => {
  test("server messages point at the exact place to fix them", () => {
    const cases: Array<[string, string]> = [
      ["No AI provider configured. Go to Settings to add an API key.", "/settings?tab=providers"],
      ["Rate limited by OpenRouter and no fallback providers available. Add more API keys in Settings, or wait and retry.", "/settings?tab=providers"],
      ["Cannot reach AI provider. Check your API key and network connection in Settings.", "/settings?tab=providers"],
      ["No LLM provider is configured: add a key in Settings", "/settings?tab=providers"],
      ["provider_key_missing: Hunter needs an API key; add it in Settings", "/settings?tab=enrichment"],
      ["HubSpot token is missing (set the token in Settings)", "/settings?tab=integrations"],
      ["SMTP connection refused", "/settings?tab=email"],
      ["Add a key in Settings", "/settings"],
    ]
    for (const [message, to] of cases) expect(nextStepFor(message)?.to).toBe(to)
  })
  test("a message that names no place gets no action", () => {
    for (const m of ["Failed to create workbook", "Saved", "", undefined, 42]) expect(nextStepFor(m)).toBeNull()
  })
})

test("toasts are imported from lib/notify so they carry their next step", () => {
  const offenders: string[] = []
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      const path = join(dir, name)
      if (statSync(path).isDirectory()) walk(path)
      else if (/\.tsx?$/.test(name) && !path.endsWith("lib/notify.ts") && !path.endsWith("ui/sonner.tsx")
        && /from "sonner"/.test(readFileSync(path, "utf8"))) offenders.push(path)
    }
  }
  walk(join(import.meta.dir, "../src"))
  expect(offenders).toEqual([])
})

describe("server-sent next steps", () => {
  test("a destination from X-Next-Step beats the wording rules", () => {
    rememberNextStep("Quota exceeded for workspace", "/settings?tab=billing", "Open billing")
    expect(nextStepFor("Could not run: Quota exceeded for workspace")).toEqual({
      label: "Open billing",
      to: "/settings?tab=billing",
    })
  })

  test("off-site destinations are ignored", () => {
    for (const to of ["https://evil.example", "//evil.example", "/\\evil.example", "settings", ""]) {
      rememberNextStep(`bad ${to}`, to, "x")
      expect(nextStepFor(`bad ${to}`)).toBeNull()
    }
    expect(safeInternalPath("/settings?tab=email")).toBe("/settings?tab=email")
  })

  test("a missing label falls back to a generic one", () => {
    rememberNextStep("Widget missing", "/sources", null)
    expect(nextStepFor("Widget missing")?.label).toBe("Fix this")
  })
})
