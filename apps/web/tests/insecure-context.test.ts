import { expect, test } from "bun:test"
import { readdirSync, readFileSync } from "node:fs"
import { join } from "node:path"
import { randomId, sha256Hex, sha256HexSync } from "../src/lib/insecure-context"

// A self-hosted install is often opened over plain HTTP on a LAN address, where
// crypto.randomUUID, crypto.subtle and navigator.clipboard are undefined.

const enc = (s: string) => new TextEncoder().encode(s)

test("sha256HexSync matches the published test vectors", () => {
  expect(sha256HexSync(enc(""))).toBe("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
  expect(sha256HexSync(enc("abc"))).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
  expect(sha256HexSync(enc("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq")))
    .toBe("248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1")
})

test("sha256HexSync agrees with crypto.subtle across block boundaries and non-ASCII text", async () => {
  for (const n of [1, 55, 56, 63, 64, 65, 119, 120, 1000, 70000]) {
    const text = "é✓".repeat(n).slice(0, n)
    expect(sha256HexSync(enc(text))).toBe(await sha256Hex(text))
  }
})

test("randomId returns a version 4 UUID without crypto.randomUUID", () => {
  const original = globalThis.crypto.randomUUID
  // @ts-expect-error simulate an insecure context
  globalThis.crypto.randomUUID = undefined
  try {
    const ids = new Set(Array.from({ length: 200 }, () => randomId()))
    expect(ids.size).toBe(200)
    for (const id of ids) expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  } finally {
    globalThis.crypto.randomUUID = original
  }
})

function files(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? files(join(dir, e.name)) : /\.(ts|tsx)$/.test(e.name) ? [join(dir, e.name)] : [])
}

test("src calls secure-context-only APIs only through lib/insecure-context", () => {
  const banned = /crypto\s*\.\s*(randomUUID|subtle)|navigator\s*\.\s*clipboard/
  const offenders: string[] = []
  for (const file of files(join(import.meta.dir, "../src"))) {
    if (file.endsWith("lib/insecure-context.ts")) continue
    readFileSync(file, "utf8").split("\n").forEach((line, i) => {
      if (banned.test(line)) offenders.push(`${file}:${i + 1}: ${line.trim().slice(0, 120)}`)
    })
  }
  expect(offenders).toEqual([])
})
