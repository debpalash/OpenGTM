// Fails when the committed typed API client no longer matches the OpenAPI
// document it is generated from.
//
//   bun scripts/check-api-client.ts          check (CI)
//   bun scripts/check-api-client.ts --write  regenerate (same as `bun run gen:api`)
//
// The document is packages/contracts/openapi.v2.yaml; the generated types are
// src/lib/api-v2/schema.ts. Exit codes: 0 in sync, 1 out of sync, 2 the
// generator failed.

import { spawnSync } from "node:child_process"
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const root = resolve(import.meta.dirname, "..")
const spec = resolve(root, "..", "..", "packages", "contracts", "openapi.v2.yaml")
const committed = join(root, "src", "lib", "api-v2", "schema.ts")
const write = process.argv.includes("--write")

const dir = mkdtempSync(join(tmpdir(), "opengtm-api-client-"))
try {
  const generated = join(dir, "schema.ts")
  const run = spawnSync(join(root, "node_modules", ".bin", "openapi-typescript"), [spec, "-o", generated], { cwd: root, encoding: "utf8" })
  if (run.status !== 0) {
    console.error(`openapi-typescript failed:\n${run.stderr}${run.stdout}${run.error ?? ""}`)
    process.exit(2)
  }
  const fresh = readFileSync(generated, "utf8")
  if (write) {
    writeFileSync(committed, fresh)
    console.log(`Wrote ${committed}`)
    process.exit(0)
  }
  let current = ""
  try { current = readFileSync(committed, "utf8") } catch { /* missing counts as out of sync */ }
  if (current !== fresh) {
    console.error("The typed API client is out of date with packages/contracts/openapi.v2.yaml.")
    console.error("Run `bun run --cwd apps/web gen:api` and commit the result.")
    const a = current.split("\n")
    const b = fresh.split("\n")
    const at = a.findIndex((line, i) => line !== b[i])
    if (at >= 0) console.error(`First difference at line ${at + 1}:\n  committed: ${a[at] ?? "(end of file)"}\n  generated: ${b[at] ?? "(end of file)"}`)
    process.exit(1)
  }
  console.log("Typed API client is in sync with the OpenAPI document.")
} finally {
  rmSync(dir, { recursive: true, force: true })
}
