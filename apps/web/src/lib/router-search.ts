// URL search serialization for TanStack Router.
//
// TanStack Router JSON-encodes search values by default (`?id=%22123%22` for
// the string "123"). OpenGTM URLs are plain `URLSearchParams` strings shared in
// links, notifications and bookmarks, so parse and stringify them exactly as
// react-router did: every value is a string, the first occurrence of a key
// wins (`URLSearchParams.get`), and spaces encode as `+`.

export type SearchRecord = Record<string, string>

export function parseSearch(searchStr: string): SearchRecord {
  const params = new URLSearchParams(searchStr.startsWith("?") ? searchStr.slice(1) : searchStr)
  const search: SearchRecord = {}
  for (const [key, value] of params) {
    if (!Object.hasOwn(search, key)) search[key] = value
  }
  return search
}

export function stringifySearch(search: Record<string, unknown>): string {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(search)) {
    if (value !== undefined && value !== null) params.set(key, String(value))
  }
  const searchStr = params.toString()
  return searchStr ? `?${searchStr}` : ""
}

// ── Route search validators ──
// Plain functions rather than a schema library: they run in the entry bundle
// before any page loads. Each keeps only its known keys, and only when the
// value is a string (always the case with parseSearch above).

export function optionalString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined
}

/** Validator for a route whose search params are all optional strings. */
export function optionalStrings<const K extends string>(...keys: K[]) {
  return (search: Record<string, unknown>): { [P in K]?: string } => {
    const result: { [P in K]?: string } = {}
    for (const key of keys) {
      const value = optionalString(search[key])
      if (value !== undefined) result[key] = value
    }
    return result
  }
}

/**
 * Link options for an app-relative href held as data (notification and Quick
 * Look destinations such as `/workbooks/wb_1?run=r_2`). Anything else is
 * passed through, so absolute URLs stay external links as with react-router.
 */
export function hrefLinkOptions(href: string): { to: string; search?: SearchRecord; hash?: string } {
  if (!href.startsWith("/") || href.startsWith("//")) return { to: href }
  const url = new URL(href, "http://opengtm.invalid")
  return { to: url.pathname, search: parseSearch(url.search), hash: url.hash ? url.hash.slice(1) : undefined }
}
