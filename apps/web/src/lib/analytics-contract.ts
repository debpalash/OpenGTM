type Data = Record<string, unknown>
const object = (value: unknown): value is Data =>
  !!value && typeof value === "object" && !Array.isArray(value)
const number = (value: unknown): value is number =>
  typeof value === "number" && Number.isFinite(value) && value >= 0
const count = (value: unknown) => number(value) && Number.isSafeInteger(value)
const nullableNumber = (value: unknown) => value === null || number(value)
const nullableCount = (value: unknown) => value === null || count(value)
const counts = (value: unknown) =>
  object(value) && Object.values(value).every(count)
const fields = (
  value: unknown,
  names: string[],
  valid: (value: unknown) => boolean
) => object(value) && names.every((name) => valid(value[name]))
const rows = (value: unknown, valid: (row: Data) => boolean) =>
  Array.isArray(value) && value.every((row) => object(row) && valid(row))
const day = (row: Data) => typeof row.day === "string"
// The shared-records fields were added after the legacy ones. An older server omits them, so they are checked only
// when present; a present but malformed block is still rejected.
const absent = (value: unknown) => value === undefined
const currentModel = (value: Data) =>
  (absent(value.records) ||
    (fields(
      value.records,
      ["companies", "people", "people_with_email", "verified_emails", "companies_needing_domain"],
      count
    ) &&
      fields(value.records, ["email_pct", "verified_pct"], number))) &&
  (absent(value.activity) ||
    (fields(
      value.activity,
      ["window_days", "workbooks", "workbooks_run", "running_workbooks", "failed_workbooks", "cells_filled", "cells_no_data"],
      count
    ) &&
      fields(value.activity, ["fill_rate_pct"], number) &&
      rows(
        (value.activity as Data).by_day,
        (row) => typeof row.date === "string" && fields(row, ["filled", "no_data"], count)
      ))) &&
  (absent(value.providers) ||
    rows(
      value.providers,
      (row) =>
        typeof row.provider === "string" &&
        fields(row, ["cells_filled", "paid_attempts"], count) &&
        fields(row, ["share_pct", "spend_usd"], number)
    )) &&
  (absent(value.spend) ||
    (fields(value.spend, ["window_days"], count) &&
      fields(value.spend, ["window_usd", "total_usd"], number) &&
      rows(
        (value.spend as Data).by_day,
        (row) => typeof row.date === "string" && number(row.usd)
      ))) &&
  (absent(value.legacy) ||
    (object(value.legacy) &&
      typeof value.legacy.present === "boolean" &&
      count(value.legacy.leads)))
const contracts = {
  overview: (value: Data) =>
    fields(
      value,
      ["total_leads", "leads_this_week", "leads_this_month"],
      count
    ) &&
    number(value.avg_score) &&
    counts(value.tiers) &&
    counts(value.email_confidence) &&
    fields(
      value.enrichment,
      [
        "total",
        "with_email",
        "with_phone",
        "with_website",
        "with_contact",
        "with_linkedin"
      ],
      count
    ) &&
    fields(
      value.enrichment,
      ["email_pct", "phone_pct", "website_pct", "contact_pct"],
      number
    ) &&
    fields(value.jobs, ["total", "completed", "failed"], count) &&
    fields(value.jobs, ["success_rate"], number) &&
    currentModel(value),
  pipeline: (value: Data) =>
    counts(value.statuses) && object(value.status_tiers),
  collection: (value: Data) =>
    number(value.avg_leads_per_job) &&
    rows(
      value.jobs_by_day,
      (row) => day(row) && fields(row, ["jobs", "leads", "completed"], count)
    ) &&
    rows(value.leads_by_day, (row) => day(row) && count(row.count)),
  enrichment: (value: Data) =>
    rows(
      value.source_quality,
      (row) =>
        typeof row.source === "string" &&
        count(row.count) &&
        nullableNumber(row.avg_score) &&
        fields(row, ["with_email", "with_phone", "with_contact"], nullableCount)
    ) &&
    rows(
      value.by_city,
      (row) =>
        (row.city === null || typeof row.city === "string") &&
        count(row.count) &&
        nullableNumber(row.avg_score)
    ) &&
    rows(
      value.score_distribution,
      (row) => typeof row.range === "string" && count(row.count)
    ),
  llm: (value: Data) =>
    fields(value, ["total_tokens", "total_calls"], count) &&
    rows(
      value.by_day,
      (row) =>
        day(row) &&
        fields(row, ["tokens", "calls"], count) &&
        typeof row.providers === "string"
    ) &&
    rows(
      value.by_provider,
      (row) =>
        typeof row.provider === "string" &&
        fields(row, ["tokens", "calls"], count)
    )
}
export function validateAnalytics(
  name: keyof typeof contracts,
  value: unknown
): void {
  if (!object(value) || !contracts[name](value))
    throw new Error(
      `The ${name} analytics response is invalid. Try loading it again.`
    )
}
