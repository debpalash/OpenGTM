import type { DestinationType } from "@/lib/api"
import { recordAPI } from "@/lib/records"
const knownTypes = new Set([
  "webhook",
  "hubspot",
  "salesforce",
  "warehouse_http",
  "meta_ads",
  "google_ads",
  "linkedin_ads",
  "instantly",
  "smartlead",
  "google_sheets",
  "airtable",
  "slack"
])
const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value)
export function validateDestinationCatalog(value: unknown): DestinationType[] {
  if (!object(value) || !Array.isArray(value.types))
    throw new Error(
      "Destination availability could not be verified. Try the catalog again."
    )
  const ids = new Set<string>()
  for (const entry of value.types) {
    if (
      !object(entry) ||
      typeof entry.id !== "string" ||
      !knownTypes.has(entry.id) ||
      ids.has(entry.id) ||
      !["beta", "supported"].includes(String(entry.maturity)) ||
      !Array.isArray(entry.capabilities) ||
      !entry.capabilities.every((v) => typeof v === "string")
    )
      throw new Error(
        "Destination availability could not be verified. Try the catalog again."
      )
    const certificate = entry.certification
    if (
      (entry.maturity === "beta" && certificate !== null) ||
      (entry.maturity === "supported" &&
        (!object(certificate) ||
          ![
            "validated_at",
            "expires_at",
            "build_sha",
            "validation_run_id",
            "evidence_url"
          ].every(
            (key) =>
              typeof certificate[key] === "string" && !!certificate[key].trim()
          ) ||
          !Number.isFinite(Date.parse(String(certificate.validated_at))) ||
          !Number.isFinite(Date.parse(String(certificate.expires_at)))))
    )
      throw new Error(
        "Destination certification could not be verified. Try the catalog again."
      )
    ids.add(entry.id)
  }
  return value.types as DestinationType[]
}
export async function fetchDestinationCatalog(
  signal?: AbortSignal
): Promise<DestinationType[]> {
  return validateDestinationCatalog(
    await recordAPI("audience-destinations/types", undefined, "GET", signal)
  )
}
