import { describe, expect, test } from "bun:test"
import { parseClipboardTable, serializeClipboardTable } from "../src/lib/workbook-clipboard"

describe("spreadsheet clipboard cells", () => {
  test("round trips notes containing record separators and quotes", () => {
    const rows = [['first\nsecond', 'tab\there', 'say "hello"'], ['\r\n', '', 'plain']]
    expect(parseClipboardTable(serializeClipboardTable(rows))).toEqual(rows)
  })
  test("reads Excel TSV with quoted multiline cells and trailing record separator", () => {
    expect(parseClipboardTable('"first\r\nsecond"\t"say ""hello"""\r\n')).toEqual([
      ['first\r\nsecond', 'say "hello"'],
    ])
  })
  test("keeps ordinary paste, blank columns and interior blank rows", () => {
    expect(parseClipboardTable('Acme\t\r\n\r\nBeta\tvalue\r\n')).toEqual([
      ['Acme', ''], [''], ['Beta', 'value'],
    ])
    expect(parseClipboardTable('')).toEqual([])
    expect(parseClipboardTable('a "quote"')).toEqual([['a "quote"']])
  })
  test("refuses malformed quoted cells before any mutations", () => {
    expect(() => parseClipboardTable('"unfinished\nnext')).toThrow('unclosed')
    expect(() => parseClipboardTable('"closed"extra')).toThrow('after a quoted cell')
  })
})
