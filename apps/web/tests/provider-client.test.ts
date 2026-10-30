import { afterEach, expect, test } from "bun:test"
import {
  fetchProviderSettings,
  ProviderWriteError,
  testProvider,
  updateProvider,
  validateProviderSettings,
  type Provider
} from "../src/features/settings/provider-client"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})
const provider: Provider = {
  id: "example",
  name: "Example",
  icon: "",
  configured: true,
  api_key_masked: "••••••••",
  base_url: "https://example.test",
  model: "saved",
  default_model: "default",
  docs_url: "https://example.test/docs",
  free_tier: "Fixture",
  is_default: true,
  openai_compatible: true
}

test("provider reads validate every field used by the editor and reject duplicate identities", () => {
  expect(
    validateProviderSettings({
      providers: [provider],
      default_provider: "example"
    }).providers
  ).toEqual([provider])
  for (const bad of [
    null,
    { providers: null },
    { providers: [provider, provider], default_provider: "example" },
    {
      providers: [{ ...provider, configured: "false" }],
      default_provider: "example"
    },
    {
      providers: [{ ...provider, default_model: null }],
      default_provider: "example"
    }
  ]) {
    expect(() => validateProviderSettings(bad)).toThrow("could not be verified")
  }
})

test("failed provider reads never become an empty or successful catalog", async () => {
  globalThis.fetch = (async () =>
    Response.json({ providers: [] }, { status: 503 })) as typeof fetch
  await expect(fetchProviderSettings()).rejects.toThrow(
    "could not be loaded (503)"
  )
})

test("provider writes send the explicit fields once and require a scoped receipt", async () => {
  const calls: Array<{ url: string; body: unknown }> = []
  globalThis.fetch = (async (url, options) => {
    calls.push({ url: String(url), body: JSON.parse(String(options?.body)) })
    return Response.json({ status: "ok", provider: "example/a" })
  }) as typeof fetch
  await updateProvider("example/a", { model: "", api_key: "synthetic-key" })
  expect(calls).toEqual([
    {
      url: "/api/settings/providers/example%2Fa",
      body: { model: "", api_key: "synthetic-key" }
    }
  ])
})

test("server and lost-response failures retain unknown outcomes without automatic retry", async () => {
  for (const status of [429, 500, 503]) {
    let calls = 0
    globalThis.fetch = (async () => {
      calls++
      return Response.json({}, { status })
    }) as typeof fetch
    try {
      await updateProvider("example", { model: "new" })
      throw new Error("unexpected success")
    } catch (error) {
      expect(error).toBeInstanceOf(ProviderWriteError)
      expect((error as ProviderWriteError).outcome).toBe("unknown")
      expect((error as Error).message).toContain(
        "Some changes may have been saved"
      )
    }
    expect(calls).toBe(1)
  }
  let calls = 0
  globalThis.fetch = (async () => {
    calls++
    throw new TypeError("synthetic lost response")
  }) as typeof fetch
  await expect(
    updateProvider("example", { set_default: true })
  ).rejects.toMatchObject({ outcome: "unknown" })
  expect(calls).toBe(1)
})

test("explicit authorization and validation refusals are distinct from uncertain writes", async () => {
  for (const status of [401, 403, 404, 413, 422]) {
    globalThis.fetch = (async () =>
      Response.json({}, { status })) as typeof fetch
    await expect(
      updateProvider("example", { model: "draft" })
    ).rejects.toMatchObject({ outcome: "refused", status })
  }
})

test("malformed or foreign success receipts never settle provider drafts", async () => {
  for (const receipt of [
    null,
    {},
    { status: "ok" },
    { status: "ok", provider: "other" },
    { status: "error", provider: "example" }
  ]) {
    globalThis.fetch = (async () => Response.json(receipt)) as typeof fetch
    await expect(
      updateProvider("example", { model: "draft" })
    ).rejects.toMatchObject({ outcome: "unknown" })
  }
})

test("provider tests require a real result rather than inventing connection success", async () => {
  globalThis.fetch = (async () =>
    Response.json({ status: "ok" })) as typeof fetch
  await expect(testProvider("example")).rejects.toThrow("could not be verified")
  globalThis.fetch = (async () =>
    Response.json({
      status: "error",
      error: "Synthetic refusal"
    })) as typeof fetch
  expect(await testProvider("example")).toEqual({
    ok: false,
    message: "Synthetic refusal"
  })
  globalThis.fetch = (async () =>
    Response.json({ status: "ok", response: "hello" })) as typeof fetch
  expect(await testProvider("example")).toEqual({ ok: true, message: "hello" })
})
