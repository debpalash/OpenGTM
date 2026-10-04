// Bundle budget check for the web app.
//
// Reads the Vite manifest (build.manifest: true) and measures the JavaScript a
// browser must download before the first render: the HTML entry chunk plus
// every chunk it statically imports (the modulepreload set). Lazily imported
// route chunks are excluded. Fails when the initial gzip size exceeds the
// budget.
//
// Usage (after `bun run build`): `bun run check:bundle`, or
//   bun scripts/check-bundle-size.ts [--dist dist] [--budget-kb 123.4] [--json]
// The budget can also come from WEB_INITIAL_JS_BUDGET_KB. Exit codes: 0 within
// budget, 1 over budget, 2 missing or invalid build output.

import { existsSync, readFileSync, readdirSync } from "node:fs"
import { join, resolve } from "node:path"
import { gzipSync } from "node:zlib"

// Budget: initial JS measured after the TanStack Router migration and the
// twenty-ui removal (235.49 kB gzip, 48 chunks), plus 5%. Raise it
// deliberately, in its own commit, with the reason.
const DEFAULT_BUDGET_GZIP_KB = 247.3

type ManifestChunk = {
  file: string
  isEntry?: boolean
  imports?: string[]
  dynamicImports?: string[]
}
type Manifest = Record<string, ManifestChunk>

function argValue(name: string): string | undefined {
  const index = process.argv.indexOf(name)
  return index >= 0 ? process.argv[index + 1] : undefined
}

const distDir = resolve(argValue("--dist") ?? join(import.meta.dirname, "..", "dist"))
const budgetKb = Number(argValue("--budget-kb") ?? process.env.WEB_INITIAL_JS_BUDGET_KB ?? DEFAULT_BUDGET_GZIP_KB)
const manifestPath = join(distDir, ".vite", "manifest.json")

if (!Number.isFinite(budgetKb) || budgetKb <= 0) {
  console.error(`Invalid budget: ${budgetKb}`)
  process.exit(2)
}
if (!existsSync(manifestPath)) {
  console.error(`No Vite manifest at ${manifestPath}. Run \`bun run build\` first.`)
  process.exit(2)
}

const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as Manifest
const sizeCache = new Map<string, { raw: number; gzip: number }>()
function sizeOf(file: string) {
  let size = sizeCache.get(file)
  if (!size) {
    const bytes = readFileSync(join(distDir, file))
    size = { raw: bytes.length, gzip: gzipSync(bytes, { level: 9 }).length }
    sizeCache.set(file, size)
  }
  return size
}

const entries = Object.entries(manifest).filter(([, chunk]) => chunk.isEntry && chunk.file.endsWith(".js"))
if (entries.length === 0) {
  console.error("The manifest has no JavaScript entry chunk.")
  process.exit(2)
}

// Entry chunk plus its transitive static imports.
const initialFiles = new Set<string>()
const visit = (key: string) => {
  const chunk = manifest[key]
  if (!chunk || initialFiles.has(chunk.file)) return
  initialFiles.add(chunk.file)
  for (const imported of chunk.imports ?? []) visit(imported)
}
for (const [key] of entries) visit(key)

const sum = (files: Iterable<string>) => {
  let raw = 0
  let gzip = 0
  for (const file of files) {
    const size = sizeOf(file)
    raw += size.raw
    gzip += size.gzip
  }
  return { raw, gzip }
}

const allJs = readdirSync(join(distDir, "assets")).filter(file => file.endsWith(".js")).map(file => `assets/${file}`)
const entryOnly = sum(entries.map(([, chunk]) => chunk.file))
const initial = sum(initialFiles)
const total = sum(allJs)
const kb = (bytes: number) => Math.round(bytes / 10) / 100 // kB = 1000 bytes, as Vite reports

const report = {
  entry: entries.map(([, chunk]) => chunk.file),
  entryChunk: { rawKb: kb(entryOnly.raw), gzipKb: kb(entryOnly.gzip) },
  initial: { chunks: initialFiles.size, rawKb: kb(initial.raw), gzipKb: kb(initial.gzip) },
  total: { chunks: allJs.length, rawKb: kb(total.raw), gzipKb: kb(total.gzip) },
  budgetGzipKb: budgetKb,
}

if (process.argv.includes("--json")) {
  console.log(JSON.stringify(report, null, 2))
} else {
  console.log(`Entry chunk:      ${report.entry.join(", ")}  ${report.entryChunk.rawKb} kB raw / ${report.entryChunk.gzipKb} kB gzip`)
  console.log(`Initial JS:       ${report.initial.chunks} chunks  ${report.initial.rawKb} kB raw / ${report.initial.gzipKb} kB gzip (entry + static imports)`)
  console.log(`Total JS:         ${report.total.chunks} chunks  ${report.total.rawKb} kB raw / ${report.total.gzipKb} kB gzip`)
  console.log(`Budget (initial): ${budgetKb} kB gzip`)
}

if (report.initial.gzipKb > budgetKb) {
  console.error(`Initial JS is ${report.initial.gzipKb} kB gzip, over the ${budgetKb} kB budget by ${Math.round((report.initial.gzipKb - budgetKb) * 100) / 100} kB.`)
  process.exit(1)
}
console.log("Bundle budget OK.")
