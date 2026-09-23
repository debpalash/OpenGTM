export interface WorkbookRun {
  job_id: number; run_id: string | null; status: string
  row_count: number | null; column_count: number | null
  selected_cell_count?: number | null
  output_attempts?: { total: number; awaiting_receipt: number; succeeded: number; failed: number; unknown: number } | null
  fill_missing: boolean; force: boolean; retry_count: number; error: string | null
  created_at: string | null; started_at: string | null; completed_at: string | null
  last_heartbeat: string | null; next_run_at: string | null
  result: { completed: number; errors: number; total: number; rows: number; stopped: boolean; recorded_at: string } | null
}
export interface WorkbookRunsPage { runs: WorkbookRun[]; has_more: boolean; next_before_id: number | null }

const count = (value: unknown) => typeof value === "number" && Number.isSafeInteger(value) && value >= 0
function validRun(run: WorkbookRun): boolean {
  const outputs = run?.output_attempts
  const validOutputs = outputs == null || (
    [outputs.total, outputs.awaiting_receipt, outputs.succeeded, outputs.failed, outputs.unknown].every(count) &&
    outputs.total === outputs.awaiting_receipt + outputs.succeeded + outputs.failed + outputs.unknown)
  return !!run && count(run.job_id) && run.job_id > 0 && typeof run.status === "string" &&
    validOutputs &&
    (run.row_count === null || count(run.row_count)) && (run.column_count === null || count(run.column_count)) &&
    (run.selected_cell_count == null || count(run.selected_cell_count)) &&
    count(run.retry_count) && typeof run.fill_missing === "boolean" && typeof run.force === "boolean" &&
    (run.result === null || (!!run.result && count(run.result.completed) && count(run.result.errors) &&
      count(run.result.total) && count(run.result.rows) && typeof run.result.stopped === "boolean" && typeof run.result.recorded_at === "string"))
}

export async function fetchWorkbookRuns(id: string, beforeId?: number, signal?: AbortSignal): Promise<WorkbookRunsPage> {
  const query = new URLSearchParams({ limit: "20" })
  if (beforeId != null) query.set("before_id", String(beforeId))
  const response = await fetch(`/api/workbooks/${encodeURIComponent(id)}/runs?${query}`, { signal })
  if (!response.ok) throw new Error("Could not load run history. Try again.")
  const data = await response.json()
  if (!Array.isArray(data?.runs) || typeof data.has_more !== "boolean" ||
      (data.has_more && (!Number.isSafeInteger(data.next_before_id) || data.next_before_id < 1)) ||
      !data.runs.every(validRun)) {
    throw new Error("The server returned invalid run history. Try again.")
  }
  return data
}

export function workbookRunLabel(run: WorkbookRun): string {
  if (run.status === "cancelled") return "Cancelled"
  if (run.status === "failed") return "Failed"
  if (run.status === "pending") return run.retry_count > 0 ? "Retry queued" : "Queued"
  if (run.status === "processing") return "Running"
  if (run.status === "completed") {
    if (run.result?.stopped) return "Stopped"
    if (run.result && run.result.errors > 0) return "Finished with cell errors"
    return "Worker finished"
  }
  return run.status
}
