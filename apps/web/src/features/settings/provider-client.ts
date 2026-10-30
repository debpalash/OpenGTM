export interface Provider {
  id: string
  name: string
  icon: string
  configured: boolean
  api_key_masked: string
  base_url: string
  model: string
  default_model: string
  docs_url: string
  free_tier: string
  is_default: boolean
  openai_compatible: boolean
}

export interface ProviderSettings {
  providers: Provider[]
  default_provider: string
}

export interface ProviderUpdate {
  api_key?: string
  model?: string
  set_default?: true
}

export function validateProviderSettings(value: unknown): ProviderSettings {
  const data = value as ProviderSettings | null
  const ids = new Set<string>()
  if (
    !data ||
    typeof data.default_provider !== "string" ||
    !Array.isArray(data.providers) ||
    data.providers.some((provider) => {
      if (
        !provider ||
        typeof provider.id !== "string" ||
        !provider.id ||
        ids.has(provider.id)
      )
        return true
      ids.add(provider.id)
      return (
        [
          "name",
          "icon",
          "api_key_masked",
          "base_url",
          "model",
          "default_model",
          "docs_url",
          "free_tier"
        ].some((key) => typeof provider[key as keyof Provider] !== "string") ||
        ["configured", "is_default", "openai_compatible"].some(
          (key) => typeof provider[key as keyof Provider] !== "boolean"
        )
      )
    })
  )
    throw new Error("Provider settings could not be verified. Try again.")
  return data
}

export async function fetchProviderSettings(
  signal?: AbortSignal
): Promise<ProviderSettings> {
  const response = await fetch("/api/settings/providers", { signal })
  if (!response.ok)
    throw new Error(
      `Provider settings could not be loaded (${response.status}). Try again.`
    )
  return validateProviderSettings(await response.json())
}

export class ProviderWriteError extends Error {
  readonly outcome: "refused" | "unknown"
  readonly status?: number
  constructor(outcome: "refused" | "unknown", status?: number) {
    super(
      outcome === "refused"
        ? `Save was refused${status ? ` (${status})` : ""}. Your draft is kept.`
        : `Save outcome unknown${status ? ` (${status})` : ""}. Some changes may have been saved. Your draft is kept.`
    )
    this.outcome = outcome
    this.status = status
  }
}

// An HTTP success alone is insufficient: only this provider's explicit receipt
// settles a draft. Transport/server/malformed-receipt failures may follow commit.
export async function updateProvider(
  id: string,
  input: ProviderUpdate
): Promise<void> {
  let response: Response
  try {
    response = await fetch(
      `/api/settings/providers/${encodeURIComponent(id)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(input)
      }
    )
  } catch {
    throw new ProviderWriteError("unknown")
  }
  if (!response.ok)
    throw new ProviderWriteError(
      [401, 403, 404, 413, 422].includes(response.status)
        ? "refused"
        : "unknown",
      response.status
    )
  try {
    const receipt = await response.json()
    if (receipt?.status !== "ok" || receipt.provider !== id) throw new Error()
  } catch {
    throw new ProviderWriteError("unknown")
  }
}

export async function testProvider(
  id: string
): Promise<{ ok: boolean; message: string }> {
  const response = await fetch(
    `/api/settings/providers/${encodeURIComponent(id)}/test`,
    { method: "POST" }
  )
  if (!response.ok)
    throw new Error(`Provider test failed (${response.status}). Try again.`)
  const data = await response.json()
  if (data?.status === "ok" && typeof data.response === "string")
    return { ok: true, message: data.response }
  if (data?.status === "error" && typeof data.error === "string")
    return { ok: false, message: data.error }
  throw new Error("The provider test result could not be verified. Try again.")
}
