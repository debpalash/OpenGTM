import { describe, expect, test } from "bun:test"
import { hrefLinkOptions, optionalString, optionalStrings, parseSearch, stringifySearch } from "../src/lib/router-search"

describe("route search validators", () => {
  test("optionalString accepts strings only", () => {
    expect(optionalString("abc")).toBe("abc")
    expect(optionalString("")).toBe("")
    expect(optionalString(undefined)).toBeUndefined()
    expect(optionalString(123)).toBeUndefined()
    expect(optionalString(["a"])).toBeUndefined()
  })
  test("optionalStrings keeps known string keys and drops the rest", () => {
    const validate = optionalStrings("id", "draft")
    expect(validate({ id: "c1", draft: "hi there", other: "x" })).toEqual({ id: "c1", draft: "hi there" })
    expect(validate({ id: 5, draft: undefined })).toEqual({})
    expect(validate({})).toEqual({})
  })
  test("validators accept what parseSearch produces", () => {
    expect(optionalStrings("q", "status", "sort", "page")(parseSearch("?q=a+b&page=2&x=1")))
      .toEqual({ q: "a b", page: "2" })
  })
})

describe("router search serialization", () => {
  test("parses every value as a string, first occurrence wins", () => {
    expect(parseSearch("?id=123&flag=true&q=a+b&q=ignored&empty=")).toEqual({ id: "123", flag: "true", q: "a b", empty: "" })
    expect(parseSearch("")).toEqual({})
  })
  test("stringifies like URLSearchParams and omits undefined values", () => {
    expect(stringifySearch({ id: "123", draft: "hello world", gone: undefined })).toBe("?id=123&draft=hello+world")
    expect(stringifySearch({})).toBe("")
  })
  test("round-trips existing URLs unchanged", () => {
    for (const search of ["?q=a+b&status=all&page=2", "?id=0f8c2b6e-1d2a-4f7e-9c1b-2a3d4e5f6a7b", "?target=acme.com&create=1"]) {
      expect(stringifySearch(parseSearch(search))).toBe(search)
    }
  })
  test("splits app hrefs into link options and leaves external URLs alone", () => {
    expect(hrefLinkOptions("/workbooks/wb_1?run=r_2#cell")).toEqual({ to: "/workbooks/wb_1", search: { run: "r_2" }, hash: "cell" })
    expect(hrefLinkOptions("/agents/job-1")).toEqual({ to: "/agents/job-1", search: {}, hash: undefined })
    expect(hrefLinkOptions("https://example.com/x")).toEqual({ to: "https://example.com/x" })
    expect(hrefLinkOptions("//evil.example/x")).toEqual({ to: "//evil.example/x" })
  })
})
