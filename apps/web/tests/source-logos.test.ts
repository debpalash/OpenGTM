import { expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import assets from "../src/lib/source-brand-assets.json"

const categories = new Set([
  "directories", "job_boards", "generic_companies_list", "generic_association",
  "generic_awards", "generic_expo", "generic_incubator", "generic_govt_tender",
  "generic_iso_certified", "generic_hiring_surge"
])
const catalog = JSON.parse(readFileSync(new URL("../../backend-go/internal/sourcecatalog/sources.json", import.meta.url), "utf8"))
const brands = assets as Record<string, { file: string }>
const manifest = JSON.parse(readFileSync(new URL("../public/source-logos/manifest.json", import.meta.url), "utf8")) as Array<{
  id: string; source?: string; file?: string; format?: string; category?: boolean
}>

test("every named source has a bundled brand mark; generic categories keep category icons", () => {
  for (const source of catalog.data_sources) {
    expect(Boolean(brands[source.id])).toBe(!categories.has(source.id))
  }
  expect(brands.firecrawl).toBeDefined()
  expect(brands.spicrawl).toBeDefined()
})

test("SVG assets are local, bounded and inert, with recorded provenance and raster exceptions", () => {
  let total = 0
  for (const [id, { file }] of Object.entries(brands)) {
    expect(file).toMatch(/^[a-z0-9_-]+\.svg$/)
    const raw = readFileSync(new URL(`../public/source-logos/${file}`, import.meta.url), "utf8")
    total += raw.length
    expect(raw).toMatch(/<svg\b/)
    expect(raw).toMatch(/viewBox="[-\d. ]+"/)
    expect(raw).not.toMatch(/<\s*(?:script|foreignObject|iframe|animate|set)\b|\son[a-z]+\s*=/i)
    for (const [, value] of raw.matchAll(/(?:href|src)="([^"]+)"/g)) {
      expect(value.startsWith("#") || value.startsWith("data:image/png;base64,")).toBe(true)
    }
    const entry = manifest.find(item => item.id === id)
    expect(entry?.source?.startsWith("https://")).toBe(true)
    if (raw.includes("data:image/png")) expect(entry?.format).toContain("PNG")
  }
  expect(total).toBeLessThan(2 * 1024 * 1024)
})
