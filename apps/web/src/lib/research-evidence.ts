/** Citation text is untrusted; only explicit web URLs may become links. */
export function citationHref(value: string): string | null {
  try {
    const url = new URL(value)
    return ["https:", "http:"].includes(url.protocol) && !url.username && !url.password ? url.href : null
  } catch { return null }
}
