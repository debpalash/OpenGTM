/** Spreadsheet TSV keeps embedded tabs/newlines inside quoted cells. */
export function serializeClipboardTable(rows: readonly (readonly string[])[]): string {
  return rows.map(row => row.map(value => /[\t\r\n"]/.test(value)
    ? `"${value.replace(/"/g, '""')}"` : value).join("\t")).join("\n")
}

export function parseClipboardTable(text: string): string[][] {
  if (!text) return []
  const rows: string[][] = []
  let row: string[] = [], cell = "", quoted = false, closed = false
  for (let index = 0; index < text.length; index++) {
    const char = text[index]
    if (quoted) {
      if (char !== '"') cell += char
      else if (text[index + 1] === '"') { cell += '"'; index++ }
      else { quoted = false; closed = true }
      continue
    }
    if (char === "\t" || char === "\n" || char === "\r") {
      row.push(cell); cell = ""; closed = false
      if (char !== "\t") {
        rows.push(row); row = []
        if (char === "\r" && text[index + 1] === "\n") index++
      }
    } else if (closed) {
      throw new Error("Clipboard contains text after a quoted cell")
    } else if (char === '"' && cell === "") quoted = true
    else cell += char
  }
  if (quoted) throw new Error("Clipboard contains an unclosed quoted cell")
  // A final record separator is conventional in spreadsheet clipboard text.
  if (row.length || cell || !/[\r\n]$/.test(text)) rows.push([...row, cell])
  return rows
}
