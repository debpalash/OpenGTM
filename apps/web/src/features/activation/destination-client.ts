import type { Audience, AudienceDestination, DestinationRun } from "@/lib/api"
import { recordAPI } from "@/lib/records"
import { recordTimestamp } from "@/lib/record-overview"

export type DestinationAction = "sync" | "retry" | "cancel"
export interface DestinationContext {
  destination: AudienceDestination
  audience: Audience
}
export interface RunPage {
  runs: DestinationRun[]
  has_more: boolean
  next_cursor: string | null
  limit: number
}
export interface DeliveryReceipt {
  id: number
  run_id: string
  destination_id: string
  lead_id: number | null
  company_id?: string
  person_id?: string
  operation: string
  status: string
  attempts: number
  external_id: string | null
  summary: string | null
  error: string | null
  delivered_at: string | null
  created_at: string
}
export const ACTIVE_RUNS = new Set(["pending", "running", "cancelling"])
const RUN_STATES = new Set([
  ...ACTIVE_RUNS,
  "completed",
  "completed_with_errors",
  "failed",
  "cancelled"
])
const DELIVERY_STATES = new Set([
  "pending",
  "in_flight",
  "success",
  "failed",
  "cancelled",
  "skipped"
])
const obj = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v)
const text = (v: unknown): v is string => typeof v === "string" && !!v.trim()
const count = (v: unknown): v is number =>
  Number.isSafeInteger(v) && (v as number) >= 0
const nullableText = (v: unknown) => v === null || typeof v === "string"
const timestamp = (v: unknown) =>
  typeof v === "string" && Number.isFinite(recordTimestamp(v))
const bad = () =>
  new Error(
    "The activation response could not be verified. Refresh before trying again."
  )

export function validateDestinationList(
  value: unknown,
  audienceID: string
): AudienceDestination[] {
  if (!Array.isArray(value)) throw bad()
  const ids = new Set<string>()
  for (const d of value) {
    if (
      !obj(d) ||
      !text(d.id) ||
      ids.has(d.id) ||
      d.audience_id !== audienceID ||
      !text(d.name) ||
      !text(d.destination_type) ||
      typeof d.enabled !== "boolean" ||
      !obj(d.config) ||
      !obj(d.field_map) ||
      !Object.values(d.field_map).every((value) => typeof value === "string") ||
      !text(d.health_status) ||
      !timestamp(d.updated_at)
    )
      throw bad()
    ids.add(d.id)
    if (d.latest_run != null) validateRun(d.latest_run, d.id)
  }
  return value as AudienceDestination[]
}

export function validateRun(
  value: unknown,
  destinationID: string,
  runID?: string
): DestinationRun {
  if (
    !obj(value) ||
    !text(value.id) ||
    (runID && value.id !== runID) ||
    value.destination_id !== destinationID ||
    !RUN_STATES.has(String(value.status)) ||
    !["attempted", "succeeded", "failed", "skipped"].every((key) =>
      count(value[key])
    ) ||
    !nullableText(value.error) ||
    !timestamp(value.created_at) ||
    !["started_at", "finished_at"].every(
      (key) => value[key] === null || timestamp(value[key])
    )
  )
    throw bad()
  return value as unknown as DestinationRun
}
export async function fetchActivationAccess(
  signal?: AbortSignal
): Promise<{ can_write: boolean }> {
  const value = await recordAPI<{ can_write: boolean }>(
    "audience-destinations/access",
    undefined,
    "GET",
    signal
  )
  if (!value || typeof value.can_write !== "boolean") throw bad()
  return value
}
export interface AudienceActivationContext {
  audience: Audience
  destinations: AudienceDestination[]
}
export type AudienceChange =
  | { kind: "refresh" }
  | {
      kind: "schedule"
      refresh_enabled?: boolean
      refresh_interval_minutes?: number
    }
export interface MembershipReceipt {
  audience: Audience
  entered: number
  exited: number
  changed: number
  unchanged: number
}
function validateAudience(value: unknown, id: string): Audience {
  if (
    !obj(value) ||
    value.id !== id ||
    !text(value.name) ||
    !obj(value.filters) ||
    typeof value.description !== "string" ||
    !timestamp(value.created_at) ||
    !["unverified", "healthy", "degraded"].includes(
      String(value.refresh_health)
    ) ||
    !nullableText(value.last_refresh_error) ||
    !count(value.consecutive_refresh_failures) ||
    !(value.next_refresh_at === null || timestamp(value.next_refresh_at)) ||
    !count(value.member_count) ||
    (value.refreshed_at !== null && !timestamp(value.refreshed_at)) ||
    !timestamp(value.updated_at) ||
    typeof value.refresh_enabled !== "boolean" ||
    !count(value.refresh_interval_minutes) ||
    !value.refresh_interval_minutes
  )
    throw bad()
  return value as unknown as Audience
}
export async function fetchAudienceActivationContext(
  audienceID: string,
  signal?: AbortSignal
): Promise<AudienceActivationContext> {
  const [destinations, audiences] = await Promise.all([
    recordAPI<unknown>(
      `audience-destinations?${new URLSearchParams({ audience_id: audienceID })}`,
      undefined,
      "GET",
      signal
    ),
    recordAPI<unknown>("audiences", undefined, "GET", signal)
  ])
  if (!Array.isArray(audiences)) throw bad()
  return {
    audience: validateAudience(
      audiences.find((value) => obj(value) && value.id === audienceID),
      audienceID
    ),
    destinations: validateDestinationList(destinations, audienceID)
  }
}
export async function fetchDestinationContext(
  destinationID: string,
  audienceID: string,
  signal?: AbortSignal
): Promise<DestinationContext> {
  const context = await fetchAudienceActivationContext(audienceID, signal)
  const destination = context.destinations.find(
    (value) => value.id === destinationID
  )
  if (!destination) throw bad()
  return { destination, audience: context.audience }
}
export function audienceReviewFingerprint(
  context: AudienceActivationContext
): string {
  const a = context.audience
  return stable({
    id: a.id,
    filters: a.filters,
    members: a.member_count,
    refreshed: a.refreshed_at,
    enabled: a.refresh_enabled,
    interval: a.refresh_interval_minutes,
    destinations: context.destinations
      .filter((d) => d.enabled)
      .sort((a, b) => a.id.localeCompare(b.id))
      .map((d) => ({
        id: d.id,
        name: d.name,
        type: d.destination_type,
        config: d.config,
        fields: Object.entries(d.field_map)
      }))
  })
}
export async function performAudienceChange(
  id: string,
  change: AudienceChange
): Promise<
  | { kind: "refresh"; receipt: MembershipReceipt }
  | { kind: "schedule"; audience: Audience }
> {
  if (change.kind === "refresh") {
    const result = await recordAPI<unknown>(
      `audiences/${encodeURIComponent(id)}/refresh`,
      {},
      "POST"
    )
    if (
      !obj(result) ||
      !["entered", "exited", "changed", "unchanged"].every((key) =>
        count(result[key])
      )
    )
      throw bad()
    return {
      kind: "refresh",
      receipt: {
        audience: validateAudience(result.audience, id),
        entered: result.entered as number,
        exited: result.exited as number,
        changed: result.changed as number,
        unchanged: result.unchanged as number
      }
    }
  }
  const { kind, ...patch } = change
  const audience = validateAudience(
    await recordAPI(`audiences/${encodeURIComponent(id)}`, patch, "PATCH"),
    id
  )
  if (
    (patch.refresh_enabled != null &&
      audience.refresh_enabled !== patch.refresh_enabled) ||
    (patch.refresh_interval_minutes != null &&
      audience.refresh_interval_minutes !== patch.refresh_interval_minutes)
  )
    throw bad()
  return { kind: "schedule", audience }
}

function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(",")}]`
  if (obj(value))
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${stable(value[key])}`)
      .join(",")}}`
  return JSON.stringify(value) ?? "null"
}
/** Cancellation targets one run; changing delivery counters does not change
 * that intent. Sync/retry also review the current audience and destination. */
export function reviewFingerprint(
  context: DestinationContext,
  action: DestinationAction
): string {
  const d = context.destination,
    a = context.audience,
    run = d.latest_run
  if (action === "cancel")
    return stable({ destination: d.id, run: run?.id, status: run?.status })
  return stable({
    destination: d.id,
    type: d.destination_type,
    name: d.name,
    enabled: d.enabled,
    config: d.config,
    fields: Object.entries(d.field_map),
    audience: a.id,
    filters: a.filters,
    count: a.member_count,
    refreshed: a.refreshed_at,
    run: run ? { id: run.id, status: run.status, failed: run.failed } : null
  })
}
export function actionUnavailable(
  context: DestinationContext,
  action: DestinationAction,
  runID?: string
): string | null {
  const d = context.destination,
    run = d.latest_run
  if (action !== "cancel" && !d.enabled)
    return "This destination is disabled. Enable it before sending records."
  if (action === "sync")
    return run && ACTIVE_RUNS.has(run.status)
      ? "A sync is already active. Review its receipt instead of starting another."
      : null
  if (!run || run.id !== runID)
    return "The latest run changed. Close this review and select the current run."
  if (
    action === "retry" &&
    !["failed", "completed_with_errors"].includes(run.status)
  )
    return "This run is no longer eligible for retry."
  if (action === "cancel" && !["pending", "running"].includes(run.status))
    return "This run is no longer eligible for cancellation."
  return null
}
export async function performDestinationAction(
  action: DestinationAction,
  destinationID: string,
  runID?: string
): Promise<DestinationRun> {
  if (action !== "sync" && !runID)
    throw new Error("Select a run before continuing.")
  const path =
    action === "sync"
      ? `${encodeURIComponent(destinationID)}/sync`
      : `runs/${encodeURIComponent(runID!)}/${action}`
  const result = await recordAPI<unknown>(
    `audience-destinations/${path}`,
    {},
    "POST"
  )
  return validateRun(
    result,
    destinationID,
    action === "sync" ? undefined : runID
  )
}
export async function fetchRunPage(
  destinationID: string,
  cursor: string | null,
  signal?: AbortSignal
): Promise<RunPage> {
  const query = new URLSearchParams({ limit: "20" })
  if (cursor) query.set("cursor", cursor)
  const value = await recordAPI<unknown>(
    `audience-destinations/${encodeURIComponent(destinationID)}/runs?${query}`,
    undefined,
    "GET",
    signal
  )
  if (
    !obj(value) ||
    !Array.isArray(value.runs) ||
    typeof value.has_more !== "boolean" ||
    !count(value.limit) ||
    value.limit === 0 ||
    value.runs.length > value.limit ||
    !nullableText(value.next_cursor) ||
    (value.has_more && (!text(value.next_cursor) || !value.runs.length)) ||
    (!value.has_more && value.next_cursor != null)
  )
    throw bad()
  if (value.has_more && value.next_cursor === cursor) throw bad()
  const runs = value.runs.map((run) => validateRun(run, destinationID))
  if (new Set(runs.map((run) => run.id)).size !== runs.length) throw bad()
  return {
    runs,
    has_more: value.has_more,
    next_cursor: value.next_cursor as string | null,
    limit: value.limit as number
  }
}
export async function fetchDeliveryPage(
  destinationID: string,
  runID: string,
  after: number | null,
  status: string,
  signal?: AbortSignal
): Promise<DeliveryReceipt[]> {
  const query = new URLSearchParams({ limit: "50" })
  if (after != null) query.set("after_id", String(after))
  if (status) query.set("status", status)
  const value = await recordAPI<unknown>(
    `audience-destinations/runs/${encodeURIComponent(runID)}/deliveries?${query}`,
    undefined,
    "GET",
    signal
  )
  if (!Array.isArray(value) || value.length > 50) throw bad()
  let previous = after ?? 0
  for (const row of value) {
    if (
      !obj(row) ||
      !count(row.id) ||
      row.id <= previous ||
      row.run_id !== runID ||
      row.destination_id !== destinationID ||
      !(row.lead_id === null || (count(row.lead_id) && row.lead_id > 0)) ||
      !["company_id", "person_id"].every(
        (key) => row[key] == null || text(row[key])
      ) ||
      !text(row.operation) ||
      !DELIVERY_STATES.has(String(row.status)) ||
      !count(row.attempts) ||
      !["external_id", "summary", "error"].every((key) =>
        nullableText(row[key])
      ) ||
      !timestamp(row.created_at) ||
      !(row.delivered_at === null || timestamp(row.delivered_at)) ||
      (status && row.status !== status)
    )
      throw bad()
    previous = row.id as number
  }
  return value as DeliveryReceipt[]
}
/** Target hints contain no URL credentials, query values, secret references or arbitrary config. */
export function destinationTarget(destination: AudienceDestination): string {
  const cfg = destination.config
  if (
    ["webhook", "warehouse_http"].includes(destination.destination_type) &&
    typeof cfg.url === "string"
  ) {
    try {
      const u = new URL(cfg.url)
      return `${u.protocol}//${u.host}`
    } catch {
      return "Target URL unavailable"
    }
  }
  const keys = [
    "custom_audience_id",
    "segment_id",
    "user_list_id",
    "campaign_id",
    "spreadsheet_id",
    "base_id"
  ]
  const key = keys.find((key) => typeof cfg[key] === "string" && cfg[key])
  return key
    ? `${key.replaceAll("_", " ")}: ${cfg[key]}`
    : destination.destination_type === "slack"
      ? "Configured Slack webhook"
      : "Configured workspace integration"
}
