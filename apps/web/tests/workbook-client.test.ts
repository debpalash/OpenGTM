import { afterEach, describe, expect, test } from "bun:test"
import { fetchWorkbookCost, setWorkbookBudget } from "../src/lib/workbook-api"
import { createWorkbook, fetchWorkbooks, fetchRunEstimate, runWorkbook, stopWorkbook } from "../src/lib/workbook-api"
import { createWorkbookFromTemplate, fetchWorkbookTemplates } from "../src/lib/workbook-templates-api"
import { fetchWorkbookViews, createWorkbookView, updateWorkbookView, saveWorkbookColumnWidth, saveWorkbookColumnOrder } from "../src/lib/workbook-api"
import { saveWorkbookColumnSettings } from "../src/lib/workbook-api"
import { deleteColumn } from "../src/lib/workbook-api"

const originalFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = originalFetch })

test("column deletion requires exact confirmation and validates removal receipt", async () => {
  const column = { id: "a/b", name: "A", type: "ai_formula" as const, width: 200, prompt: "Confirmed" }
  globalThis.fetch = (async (url, init) => {
    expect(String(url)).toContain("/book%2Fone/columns/a%2Fb")
    expect(init?.method).toBe("DELETE")
    expect(JSON.parse(String(init?.body))).toEqual({ expected_column: column })
    return Response.json({ id: "book/one", columns_config: [] })
  }) as typeof fetch
  await deleteColumn("book/one", "a/b", column)
  await expect(deleteColumn("book", "other", column)).rejects.toThrow("identity")
  for (const receipt of [{ id: "other", columns_config: [] }, { id: "book", columns_config: [column] }, { id: "book" }]) {
    respond(receipt)
    await expect(deleteColumn("book", "a/b", column)).rejects.toThrow("not confirmed")
  }
  respond({}, 409)
  await expect(deleteColumn("book", "a/b", column)).rejects.toThrow("changed since confirmation")
  respond({ detail: "Column is referenced by: Consumer. Update those references before deleting." }, 409)
  await expect(deleteColumn("book", "a/b", column)).rejects.toThrow("referenced by: Consumer")
})
function respond(payload: unknown, status = 200) {
  globalThis.fetch = (async () => Response.json(payload, { status })) as typeof fetch
}

test("column settings send only changed/expected fields and validate the receipt", async () => {
  const changes = { tools: [], reactive: false, policy: { max_steps: 0, max_cost_usd: 0 } }
  const expected = { tools: ["paid"], reactive: null, policy: null }
  globalThis.fetch = (async (url, init) => {
    expect(String(url)).toContain("/book%2Fone/columns/agent%2Fone/settings")
    expect(init?.method).toBe("PATCH")
    expect(JSON.parse(String(init?.body))).toEqual({ changes, expected })
    return Response.json({ column_id: "agent/one", changes: { ...changes, policy: { max_cost_usd: 0, max_steps: 0 } } })
  }) as typeof fetch
  expect((await saveWorkbookColumnSettings("book/one", "agent/one", changes, expected)).changes).toEqual(changes)
  for (const receipt of [{ column_id: "other", changes }, { column_id: "agent", changes: {} },
    { column_id: "agent", changes: { ...changes, reactive: true } }]) {
    respond(receipt)
    await expect(saveWorkbookColumnSettings("book", "agent", changes, expected)).rejects.toThrow("not confirmed")
  }
  respond({}, 409)
  await expect(saveWorkbookColumnSettings("book", "agent", changes, expected)).rejects.toThrow("changed elsewhere")
  respond({ detail: "Rename breaks column-name references in: Research. Change those references to stable column IDs first." }, 409)
  await expect(saveWorkbookColumnSettings("book", "agent", changes, expected)).rejects.toThrow("stable column IDs")
  respond({}, 503)
  await expect(saveWorkbookColumnSettings("book", "agent", changes, expected)).rejects.toThrow("could not be saved")
})

test("run estimate distinguishes missing, incomplete and invalid price coverage", async () => {
  const base = { rows: 1, best_usd: 0, worst_usd: 0, breakdown: [], note: "Catalog only" }
  for (const coverage of [{}, { unknown_providers: [], catalog_complete: true },
    { unknown_providers: ["unknown_lookup"], catalog_complete: false }]) {
    respond({ ...base, ...coverage })
    expect(await fetchRunEstimate("book")).toEqual({ ...base, ...coverage })
  }
  for (const coverage of [{ unknown_providers: [] }, { catalog_complete: true },
    { unknown_providers: ["unknown"], catalog_complete: true },
    { unknown_providers: [4], catalog_complete: false },
    { unknown_providers: [" "], catalog_complete: false }]) {
    respond({ ...base, ...coverage })
    await expect(fetchRunEstimate("book")).rejects.toThrow("invalid estimate coverage")
  }
})

test("budget balances reject malformed exposure but allow over-cap headroom", async () => {
  const cost = { budget_max_usd: 1, budget_spent_usd: 0.9, reserved_usd: 0.2, uncertain_usd: 0.1,
    remaining_usd: -0.2, unlimited: false, accounting_basis: "catalog_estimates_and_recorded_charges" }
  respond(cost)
  expect(await fetchWorkbookCost("book")).toEqual(cost)
  for (const bad of [{}, { ...cost, reserved_usd: undefined }, { ...cost, uncertain_usd: "0" },
    { ...cost, budget_spent_usd: -1 }, { ...cost, remaining_usd: null }, { ...cost, unlimited: true },
    { ...cost, accounting_basis: "invoice" }]) {
    respond(bad)
    await expect(fetchWorkbookCost("book")).rejects.toThrow("invalid budget")
  }
  respond({ ...cost, budget_max_usd: 0, unlimited: true, remaining_usd: null })
  expect((await fetchWorkbookCost("book")).unlimited).toBe(true)
})

test("budget save rejects invalid amounts and mismatched receipts", async () => {
  let calls = 0
  globalThis.fetch = (async () => { calls++; return Response.json({}) }) as typeof fetch
  for (const amount of [-1, NaN, Infinity]) await expect(setWorkbookBudget("book", amount)).rejects.toThrow("nonnegative finite")
  expect(calls).toBe(0)
  respond({ budget_max_usd: 2, budget_spent_usd: 0 })
  await expect(setWorkbookBudget("book", 1)).rejects.toThrow("not confirmed")
  respond({ budget_max_usd: 1, budget_spent_usd: 0.2 })
  expect(await setWorkbookBudget("book", 1)).toEqual({ budget_max_usd: 1, budget_spent_usd: 0.2 })
})

test("column order sends only identities and requires an exact receipt", async () => {
  globalThis.fetch = (async (url, options) => {
    expect(String(url)).toEndWith("/api/workbooks/book%2Fone/columns/order")
    expect(options?.method).toBe("PATCH")
    expect(JSON.parse(String(options?.body))).toEqual({ column_ids: ["b", "a"], expected_column_ids: ["a", "b"] })
    return Response.json({ column_ids: ["b", "a"] })
  }) as typeof fetch
  await saveWorkbookColumnOrder("book/one", ["b", "a"], ["a", "b"])
  for (const receipt of [{}, { column_ids: ["a", "b"] }, { column_ids: ["b"] }]) {
    respond(receipt)
    await expect(saveWorkbookColumnOrder("one", ["b", "a"], ["a", "b"])).rejects.toThrow("not confirmed")
  }
  respond({}, 409)
  await expect(saveWorkbookColumnOrder("one", ["b", "a"], ["a", "b"])).rejects.toThrow("Column order changed")
  respond({}, 503)
  await expect(saveWorkbookColumnOrder("one", ["b", "a"], ["a", "b"])).rejects.toThrow("not saved")
})

test("saved views reject malformed or mismatched success responses", async () => {
  const view = { id: "view", workbook_id: "one", name: "Partners", config: { filters: [], sort: [], hidden_columns: [] } }
  for (const bad of [{}, { ...view, workbook_id: "other" }, { ...view, config: { filters: null } }]) {
    respond(bad)
    await expect(createWorkbookView("one", { name: "Partners" })).rejects.toThrow("invalid saved view")
    respond({ views: [bad] })
    await expect(fetchWorkbookViews("one")).rejects.toThrow("invalid saved view")
  }
  respond(view)
  await expect(updateWorkbookView("one", "different", { name: "Partners" })).rejects.toThrow("different saved view")
  respond(view)
  expect(await createWorkbookView("one", { name: "Partners" })).toEqual(view)
})

test("width persistence requires an exact server receipt", async () => {
  respond({ detail: "Unavailable" }, 503)
  await expect(saveWorkbookColumnWidth("one", "company", 300)).rejects.toThrow("not saved")
  respond({ column_id: "other", width: 300 })
  await expect(saveWorkbookColumnWidth("one", "company", 300)).rejects.toThrow("not confirmed")
  respond({ column_id: "company", width: 300 })
  expect(await saveWorkbookColumnWidth("one", "company", 300)).toEqual({ column_id: "company", width: 300 })
})

describe("run review contracts", () => {
  test("does not claim stop success on HTTP errors", async () => {
    respond({ detail: "Unavailable" }, 503)
    await expect(stopWorkbook("one")).rejects.toThrow("Could not stop")
  })
  test("network failures warn of an unknown run outcome", async () => {
    globalThis.fetch = (async () => { throw new TypeError("Failed to fetch") }) as typeof fetch
    await expect(runWorkbook("one")).rejects.toThrow("Check activity")
  })
  test("does not reinterpret an estimate failure as a free run", async () => {
    respond({ detail: "Unavailable" }, 503)
    await expect(fetchRunEstimate("one")).rejects.toThrow("Could not load")
  })
  test("rejects malformed and inverted estimates", async () => {
    for (const estimate of [{ rows: 1 }, { rows: 1, best_usd: 2, worst_usd: 1, breakdown: [], note: "" }]) {
      respond(estimate)
      await expect(fetchRunEstimate("one")).rejects.toThrow("invalid run estimate")
    }
  })
  test("forwards the exact view/search to the estimate", async () => {
    globalThis.fetch = (async (input) => {
      const url = new URL(String(input), "http://fixture")
      expect(url.searchParams.get("view_id")).toBe("view/one")
      expect(url.searchParams.get("search")).toBe("Acme & sons")
      return Response.json({ rows: 3, best_usd: 0, worst_usd: 0.01, breakdown: [], note: "Catalog only" })
    }) as typeof fetch
    expect((await fetchRunEstimate("one", "view/one", " Acme & sons ")).rows).toBe(3)
  })
  test("preserves actionable structured billing errors", async () => {
    respond({ detail: { message: "Insufficient workspace credits" } }, 402)
    await expect(runWorkbook("one")).rejects.toThrow("Insufficient workspace credits")
  })
  test("rejects missing run receipts without claiming success", async () => {
    respond({ ok: true })
    await expect(runWorkbook("one")).rejects.toThrow("Check activity")
  })
  test("sends the reviewed count and query without implicit force or selected IDs", async () => {
    globalThis.fetch = (async (_input, init) => {
      expect(JSON.parse(String(init?.body))).toEqual({ expected_rows: 3, view_id: "view1", search: "Acme", fill_missing: true })
      return Response.json({ status: "started", total_jobs: 3, message: "Queued" })
    }) as typeof fetch
    await runWorkbook("one", { expected_rows: 3, view_id: "view1", search: "Acme", fill_missing: true })
  })
})

describe("workbook list and creation contracts", () => {
  test("rejects an invalid list rather than rendering it as empty", async () => {
    respond({ detail: "Unexpected error" })
    await expect(fetchWorkbooks()).rejects.toThrow("invalid workbook list")
  })
  test("rejects malformed rows", async () => {
    respond({ workbooks: [{ id: "one", name: "Incomplete" }] })
    await expect(fetchWorkbooks()).rejects.toThrow("invalid workbook list")
  })
  test("accepts an empty list", async () => {
    respond({ workbooks: [], total: 0 })
    expect(await fetchWorkbooks()).toEqual({ workbooks: [], total: 0 })
  })
  test("does not navigate to an undefined created workbook", async () => {
    respond({ ok: true })
    await expect(createWorkbook({ name: "Test" })).rejects.toThrow("workbook ID")
  })
  test("template creation rejects HTTP errors and preserves the reason", async () => {
    respond({ detail: "Editor access required" }, 403)
    await expect(createWorkbookFromTemplate("example")).rejects.toThrow("Editor access required")
  })
  test("template creation requires a real ID", async () => {
    respond({ id: " " })
    await expect(createWorkbookFromTemplate("example")).rejects.toThrow("workbook ID")
  })
  test("encodes a template ID and sends one POST", async () => {
    const calls: Array<[string, string | undefined]> = []
    globalThis.fetch = (async (url, options) => {
      calls.push([String(url), options?.method])
      return Response.json({ id: "created" })
    }) as typeof fetch
    expect(await createWorkbookFromTemplate("a/b")).toEqual({ id: "created" })
    expect(calls).toEqual([["/api/templates/a%2Fb/create", "POST"]])
  })
  test("template list validates its response", async () => {
    respond({ templates: [{ id: 1 }] })
    await expect(fetchWorkbookTemplates("sales")).rejects.toThrow("invalid template")
  })
})
