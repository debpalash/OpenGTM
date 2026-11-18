import { expect, test } from "bun:test"
import { readdirSync, readFileSync } from "node:fs"
import { join } from "node:path"

// ESLint cannot parse TypeScript here (typescript-eslint does not support TS 7),
// so this enforces the confirm/prompt/alert ban on src/**/*.{ts,tsx}.
// Native calls take a string or template argument; the useConfirm()/usePrompt()
// results take an options object, and `confirm()` with no argument is a local function.
const NATIVE = /(?<![\w.$])(?:(?:window|globalThis|self)\s*\.\s*)?(?:confirm|prompt|alert)\s*\(\s*(?![){\s])/

function files(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? files(join(dir, e.name)) : /\.(ts|tsx)$/.test(e.name) ? [join(dir, e.name)] : [])
}

function stripComments(src: string) {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:"'`\\])\/\/.*$/gm, "$1")
}

test("detector flags native dialogs and ignores the in-app API", () => {
  expect(NATIVE.test("if (!confirm(`Delete ${x}?`)) return")).toBe(true)
  expect(NATIVE.test('window.prompt("Name:")')).toBe(true)
  expect(NATIVE.test("alert('hi')")).toBe(true)
  expect(NATIVE.test("await confirm({ title })")).toBe(false)
  expect(NATIVE.test("void confirm()")).toBe(false)
  expect(NATIVE.test("yupchLibrary.prompt({")).toBe(false)
})

test("src has no native blocking dialogs (use useConfirm()/usePrompt())", () => {
  const offenders: string[] = []
  for (const file of files(join(import.meta.dir, "../src"))) {
    stripComments(readFileSync(file, "utf8")).split("\n").forEach((line, i) => {
      if (NATIVE.test(line)) offenders.push(`${file}:${i + 1}: ${line.trim().slice(0, 120)}`)
    })
  }
  expect(offenders).toEqual([])
})
