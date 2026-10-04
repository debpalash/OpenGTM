// Normalizes the evidence the Go host attaches to every plugin result, so the
// UI can show where each record came from regardless of runtime:
//
//   declarative scraper  {url, fetched_at, status, page, items, fields{field: selector}, body_sha256}
//   declarative provider {source: {source_url, method, status, fetched_at, content_type, bytes, sha256,
//                         mappings{field: expression}}, confidence, cost_usd}
//   wasm                 {fetches: [{url, status, fetched_at, content_type, bytes, sha256}], plugin: <self-reported>}
//
// Unknown shapes still surface through `raw`.

export interface EvidenceSource {
  url?: string
  method?: string
  status?: number
  fetchedAt?: string
  contentType?: string
  bytes?: number
  sha256?: string
  page?: number
}

export interface EvidenceSelector {
  field: string
  expression: string
}

export interface EvidenceView {
  sources: EvidenceSource[]
  /** The selector that split the page into records (scrapers). */
  itemsSelector?: string
  /** Field → selector (scrapers) or JSON mapping (providers). */
  selectors: EvidenceSelector[]
  selectorKind?: "selector" | "mapping"
  confidence?: number
  costUsd?: number
  /** Evidence a WebAssembly module reported about itself (informational). */
  pluginReported?: unknown
  raw: unknown
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value)
const str = (value: unknown) => typeof value === "string" && value ? value : undefined
const num = (value: unknown) => typeof value === "number" && Number.isFinite(value) ? value : undefined

function source(value: Record<string, unknown>): EvidenceSource | null {
  const out: EvidenceSource = {
    url: str(value.url) ?? str(value.source_url),
    method: str(value.method),
    status: num(value.status),
    fetchedAt: str(value.fetched_at),
    contentType: str(value.content_type),
    bytes: num(value.bytes),
    sha256: str(value.sha256) ?? str(value.body_sha256),
    page: num(value.page),
  }
  return Object.values(out).some(v => v !== undefined) ? out : null
}

function selectorList(value: unknown): EvidenceSelector[] {
  if (!isRecord(value)) return []
  return Object.entries(value)
    .filter((entry): entry is [string, string] => typeof entry[1] === "string")
    .map(([field, expression]) => ({ field, expression }))
    .sort((a, b) => a.field.localeCompare(b.field))
}

export function normalizeEvidence(evidence: unknown): EvidenceView {
  const view: EvidenceView = { sources: [], selectors: [], raw: evidence }
  if (!isRecord(evidence)) return view

  // Provider: the fetch lives under `source`; confidence and cost beside it.
  if (isRecord(evidence.source)) {
    const s = source(evidence.source)
    if (s) view.sources.push(s)
    view.selectors = selectorList(evidence.source.mappings)
    if (view.selectors.length) view.selectorKind = "mapping"
  }
  // WebAssembly: every host fetch made during the call.
  if (Array.isArray(evidence.fetches)) {
    for (const fetch of evidence.fetches) {
      const s = isRecord(fetch) ? source(fetch) : null
      if (s) view.sources.push(s)
    }
    if (evidence.plugin !== undefined && evidence.plugin !== null) view.pluginReported = evidence.plugin
  }
  // Scraper (or any flat evidence): the page itself.
  if (!view.sources.length && (str(evidence.url) || str(evidence.source_url))) {
    const s = source(evidence)
    if (s) view.sources.push(s)
  }
  if (!view.selectors.length) {
    view.selectors = selectorList(evidence.fields)
    if (view.selectors.length) view.selectorKind = "selector"
  }
  view.itemsSelector = str(evidence.items)
  view.confidence = num(evidence.confidence)
  view.costUsd = num(evidence.cost_usd)
  return view
}

/** Only http(s) URLs become links; anything else renders as text. */
export function safeHttpUrl(value: unknown): string | null {
  if (typeof value !== "string") return null
  try {
    const url = new URL(value)
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : null
  } catch {
    return null
  }
}

/** One-line evidence summary for a collapsed result row. */
export function evidenceSummary(view: EvidenceView): string {
  const first = view.sources[0]
  if (!first?.url) return view.raw == null ? "No evidence" : "Evidence attached"
  let host = first.url
  try { host = new URL(first.url).host } catch { /* keep the raw text */ }
  return view.sources.length > 1 ? `${host} +${view.sources.length - 1}` : host
}
