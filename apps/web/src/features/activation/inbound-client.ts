import { recordAPI } from "@/lib/records"

export interface InboundReceipt {
  id: string
  external_event_id: string
  lead_id: number | null
  status: string
  applied_fields: string[]
}
export interface InboundPage {
  receipts: InboundReceipt[]
  next_cursor: string | null
}
export async function fetchInboundPage(
  id: string,
  cursor: string | null,
  signal?: AbortSignal
): Promise<InboundPage> {
  const query = new URLSearchParams()
  if (cursor) query.set("cursor", cursor)
  const result = await recordAPI<InboundPage>(
    `audience-destinations/${encodeURIComponent(id)}/inbound-receipts?${query}`,
    undefined,
    "GET",
    signal
  )
  if (
    !result ||
    !Array.isArray(result.receipts) ||
    (result.next_cursor != null && typeof result.next_cursor !== "string") ||
    result.receipts.some(
      (receipt) =>
        !receipt ||
        typeof receipt.id !== "string" ||
        !receipt.id ||
        typeof receipt.external_event_id !== "string" ||
        typeof receipt.status !== "string" ||
        (receipt.lead_id !== null && !Number.isSafeInteger(receipt.lead_id)) ||
        !Array.isArray(receipt.applied_fields) ||
        receipt.applied_fields.some((field) => typeof field !== "string")
    )
  )
    throw new Error("Inbound history could not be verified. Try again.")
  return result
}
export async function rotateInboundToken(id: string): Promise<string> {
  const response = await recordAPI<{ token: string }>(
    `audience-destinations/${encodeURIComponent(id)}/inbound-token`,
    {},
    "POST"
  )
  if (!response || typeof response.token !== "string" || !response.token.trim())
    throw new Error(
      "The token response could not be verified. Review the setup before rotating again."
    )
  return response.token
}
