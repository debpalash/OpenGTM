import { expect, test } from "bun:test"
import { isStaleBuildError } from "../src/lib/stale-build"

test("recognises a page file missing after an upgrade", () => {
  expect(isStaleBuildError(new TypeError("Failed to fetch dynamically imported module: http://h/assets/workbook-editor-Cq2Pfait.js"))).toBe(true)
  expect(isStaleBuildError(new TypeError("Importing a module script failed."))).toBe(true)
  expect(isStaleBuildError(new Error("error loading dynamically imported module"))).toBe(true)
  expect(isStaleBuildError(new Error("Unable to preload CSS for /assets/x.css"))).toBe(true)
})
test("leaves ordinary errors alone", () => {
  expect(isStaleBuildError(new TypeError("crypto.randomUUID is not a function"))).toBe(false)
  expect(isStaleBuildError(undefined)).toBe(false)
})
