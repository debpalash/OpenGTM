export interface ImportDraft {
  key: string
  hash: string
  filename: string
  source: string
  mapping: Record<string, string>
}
const MAX_METADATA = 65536
export function readImportDraft(raw: string | null): ImportDraft | null {
  try {
    if (!raw || raw.length > MAX_METADATA) return null
    const value: unknown = JSON.parse(raw)
    if (!value || typeof value !== "object") return null
    const d = value as Record<string, unknown>
    if (
      typeof d.key !== "string" ||
      !d.key ||
      d.key.length > 200 ||
      typeof d.hash !== "string" ||
      !/^[a-f0-9]{64}$/.test(d.hash) ||
      typeof d.filename !== "string" ||
      d.filename.length > 512 ||
      typeof d.source !== "string" ||
      !d.source.trim() ||
      d.source.length > 200 ||
      !d.mapping ||
      typeof d.mapping !== "object" ||
      Array.isArray(d.mapping) ||
      !Object.values(d.mapping).every((item) => typeof item === "string")
    )
      return null
    return d as unknown as ImportDraft
  } catch {
    return null
  }
}
export function encodeImportDraft(draft: ImportDraft): string {
  const raw = JSON.stringify(draft)
  if (!readImportDraft(raw))
    throw new Error(
      "Reload recovery is unavailable for this batch. Keep this tab open and check the batch status before retrying."
    )
  return raw
}
