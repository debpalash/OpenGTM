import { expect, test } from "bun:test"
import { citationHref } from "../src/lib/research-evidence"

test("only explicit web citations without credentials become links", () => {
  expect(citationHref("https://example.com/team")).toBe("https://example.com/team")
  for (const value of ["javascript:alert(1)", "data:text/html,test", "file:///etc/passwd", "//example.com", "/team", "https://user:secret@example.com"]) {
    expect(citationHref(value)).toBeNull()
  }
})
