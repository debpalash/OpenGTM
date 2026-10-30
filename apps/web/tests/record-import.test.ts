import { afterEach, expect, test } from "bun:test"
import {
  inspectRecordImport,
  recordImportAPI,
  fetchImportReceipt,
  validateImportResult,
  validateImportInspection,
  ImportRequestError,
  type ImportColumn
} from "../src/lib/record-import"
import {
  mappingGroup,
  mappingIssues,
  mappingWarnings
} from "../src/features/records/import/mapping"
import {
  encodeImportDraft,
  readImportDraft
} from "../src/features/records/import/recovery"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})
const column = (name: string, target: string): ImportColumn => ({
  name,
  target,
  match: "matched",
  precision_sensitive: false,
  samples: []
})
const receipt = {
  preview: false,
  replayed: false,
  total: 2,
  imported: 1,
  rejected: 1,
  mapping: { Name: "company.name" },
  warnings: [],
  rows: [
    { row: 1, status: "accepted", id: "company-1" },
    { row: 2, status: "rejected", error: "identity conflict" }
  ]
}

test("ambiguous suggestions identify both columns without silently ignoring source values", () => {
  const columns = [
    column("Company", "company.name"),
    column("Name", "company.name"),
    column("Email", "person.email")
  ]
  const issues = mappingIssues(columns, {}, "company")
  expect(issues.get("Company")).toContain("Name")
  expect(issues.get("Name")).toContain("Company")
  expect(issues.get("Email")).toContain("cannot write")
  expect(
    mappingIssues(columns, { Name: "company.legal_name", Email: "" }, "company")
      .size
  ).toBe(0)
  expect(
    mappingIssues(
      columns,
      { Company: "", Name: "custom", Email: "" },
      "company"
    ).get("Name")
  ).toContain("Use company.field")
})

test("mapping ownership separates custom, employment and ignored fields and warns about identifiers", () => {
  expect(mappingGroup("company_id", "person")).toBe("Company")
  expect(mappingGroup("external_id", "person")).toBe("Person")
  expect(mappingGroup("employment.title", "person")).toBe("Employment")
  expect(mappingGroup("company.territory", "person")).toBe("Custom")
  expect(mappingGroup("", "person")).toBe("Ignored")
  expect(
    mappingWarnings(
      [column("Name", "person.full_name"), column("Title", "employment.title")],
      {},
      "person"
    )
  ).toHaveLength(2)
  expect(
    mappingWarnings(
      [column("Email", "person.email"), column("Company", "company.domain")],
      {},
      "person"
    )
  ).toEqual([])
})

test("inspection and preview transmit raw numeric JSON identically without a JavaScript number round-trip", async () => {
  const calls: string[] = []
  globalThis.fetch = (async (url, options) => {
    calls.push(String(options?.body))
    return String(url).endsWith("/inspect")
      ? Response.json({
          record_type: "company",
          rows: 2,
          columns: [column("ID", "external_id")]
        })
      : Response.json({ ...receipt, preview: true })
  }) as typeof fetch
  const request = {
    record_type: "company" as const,
    source: "clay:exact",
    idempotency_key: "same-key"
  }
  const raw = '[{"ID":9007199254740993},{"ID":"0000000000000012"}]'
  await inspectRecordImport(request, raw)
  await recordImportAPI(request, true, raw)
  expect(calls).toHaveLength(2)
  for (const body of calls) {
    expect(body).toContain(raw)
    expect(body).toContain('"idempotency_key":"same-key"')
  }
})

test("malformed success responses cannot become import receipts", () => {
  expect(validateImportResult(receipt, false).rejected).toBe(1)
  for (const bad of [
    null,
    { ...receipt, preview: true },
    { ...receipt, imported: 2 },
    { ...receipt, rows: [receipt.rows[0], receipt.rows[0]] },
    { ...receipt, rows: [{ row: 1, status: "accepted" }, receipt.rows[1]] },
    { ...receipt, warnings: [null] }
  ])
    expect(() => validateImportResult(bad, false)).toThrow(
      "receipt could not be verified"
    )
})

test("source inspection rejects wrong record kind, duplicate columns and unsafe samples", () => {
  const valid = {
    record_type: "company",
    rows: 1,
    columns: [
      {
        ...column("ID", "external_id"),
        samples: [{ row: 1, value: "9007199254740993", truncated: false }]
      }
    ]
  }
  expect(
    validateImportInspection(valid, "company").columns[0].samples[0].value
  ).toBe("9007199254740993")
  for (const bad of [
    { ...valid, record_type: "person" },
    { ...valid, columns: [...valid.columns, ...valid.columns] },
    {
      ...valid,
      columns: [
        {
          ...valid.columns[0],
          samples: [{ row: 2, value: "wrong row", truncated: false }]
        }
      ]
    }
  ])
    expect(() => validateImportInspection(bad, "company")).toThrow(
      "could not be verified"
    )
})

test("uncertain writes retain their key and receipt recovery reads once without replaying a mutation", async () => {
  const paths: string[] = [],
    bodies: string[] = []
  globalThis.fetch = (async (url, options) => {
    paths.push(String(url))
    bodies.push(String(options?.body ?? ""))
    if (options?.method === "POST") throw new TypeError("Response lost")
    return Response.json(receipt)
  }) as typeof fetch
  const request = {
    record_type: "company" as const,
    source: "clay:one",
    idempotency_key: "persistent-key",
    csv: "Name\nA\n"
  }
  await expect(recordImportAPI(request, false)).rejects.toThrow("Response lost")
  const result = await fetchImportReceipt(request.idempotency_key)
  expect(result.replayed).toBe(true)
  expect(paths).toEqual([
    "/api/record-imports",
    "/api/record-imports/persistent-key"
  ])
  expect(bodies[0]).toContain('"idempotency_key":"persistent-key"')
  expect(bodies[1]).toBe("")
})

test("missing receipts remain unknown and known write refusals are distinguished from server errors", async () => {
  globalThis.fetch = (async () =>
    Response.json({ detail: "No receipt" }, { status: 404 })) as typeof fetch
  await expect(fetchImportReceipt("unknown")).rejects.toThrow(
    "may still be running"
  )
  expect(new ImportRequestError(422, "bad row").outcome).toBe("refused")
  expect(new ImportRequestError(503, "offline").outcome).toBe("unknown")
  expect(new ImportRequestError(409, "different payload").outcome).toBe(
    "unknown"
  )
})

test("recovery stores only bounded metadata and preserves exact mappings including ignored columns", () => {
  const draft = {
    key: "batch-key",
    hash: "a".repeat(64),
    filename: "clay.csv",
    source: "clay:prospects",
    mapping: { ID: "external_id", Name: "company.name", Ignore: "" }
  }
  expect(readImportDraft(encodeImportDraft(draft))).toEqual(draft)
  expect(
    readImportDraft(JSON.stringify({ ...draft, mapping: { Name: null } }))
  ).toBeNull()
  expect(
    readImportDraft(JSON.stringify({ ...draft, hash: "wrong" }))
  ).toBeNull()
  expect(() =>
    encodeImportDraft({ ...draft, mapping: { name: "x".repeat(66000) } })
  ).toThrow("Reload recovery is unavailable")
  expect(readImportDraft("unparseable")).toBeNull()
})
