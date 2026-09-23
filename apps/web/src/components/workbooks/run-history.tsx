import { useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { History } from "lucide-react"
import { Button, Dialog } from "@/design-system/primitives"
import { fetchWorkbookRuns, workbookRunLabel } from "@/lib/workbook-runs"
import "./run-history.css"

function stamp(value: string | null) {
  return value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : "Not recorded"
}

export function WorkbookRunHistory({ workbookId }: { workbookId: string }) {
  const [open, setOpen] = useState(false)
  const query = useInfiniteQuery({
    queryKey: ["workbooks", "detail", workbookId, "runs"],
    initialPageParam: undefined as number | undefined,
    queryFn: ({ pageParam, signal }) => fetchWorkbookRuns(workbookId, pageParam, signal),
    getNextPageParam: page => page.has_more ? page.next_before_id ?? undefined : undefined,
    enabled: open, retry: false,
    refetchInterval: open ? 5000 : false,
  })
  const runs = query.data?.pages.flatMap(page => page.runs) ?? []
  return <Dialog.Root open={open} onOpenChange={setOpen}>
    <Dialog.Trigger render={<Button startIcon={<History aria-hidden="true" className="size-4" />} />}>Run history</Dialog.Trigger>
    <Dialog.Popup size="lg" className="gtm-run-history">
      <Dialog.Header><Dialog.Title>Workbook run history</Dialog.Title><Dialog.Description>Durable queue receipts. Worker completion does not mean every cell succeeded or every lead was verified.</Dialog.Description></Dialog.Header>
      <Dialog.Body className="gtm-run-history-body">
        {query.isPending && <p role="status">Loading runs…</p>}
        {query.isError && <div role="alert"><p>{query.error.message}</p><Button disabled={query.isFetching} onClick={() => void query.refetch()}>Retry run history</Button></div>}
        {!query.isPending && !query.isError && !runs.length && <p>No workbook runs recorded yet.</p>}
        <ol className="gtm-run-list">{runs.map(run => <li key={run.job_id}>
          <div className="gtm-run-heading"><h3>Run #{run.job_id}</h3><span role="status"><span className="sr-only">Run #{run.job_id}: </span><span>{workbookRunLabel(run)}</span></span></div>
          <p className="gtm-run-meta">{run.row_count ?? "Unknown"} rows · {run.column_count ?? "Unknown"} columns · {run.fill_missing ? "Fill missing" : "Normal run"}{run.force ? " · Forced re-run" : ""}</p>
          <p className="gtm-run-meta">{run.selected_cell_count == null ? "Selected cell count not recorded." : `${run.selected_cell_count} selected cells`}</p>
          <p className="gtm-run-meta">Queued {stamp(run.created_at)} · Retries: {run.retry_count}</p>
          {run.result ? <p className="gtm-run-outcome">Reported cells: {run.result.completed} complete · {run.result.errors} {run.result.errors === 1 ? "error" : "errors"} · {run.result.total} planned</p> : <p className="gtm-run-meta">No cell outcome recorded for this attempt.</p>}
          {run.error && <p className="gtm-run-error">{run.error}</p>}
          {run.output_attempts ? <>
            <p className="gtm-run-meta">Output receipts: {run.output_attempts.succeeded} succeeded · {run.output_attempts.failed} failed · {run.output_attempts.awaiting_receipt} awaiting receipt · {run.output_attempts.unknown} unknown</p>
            {(run.output_attempts.awaiting_receipt + run.output_attempts.failed + run.output_attempts.unknown > 0) &&
              <p className="gtm-run-error">Check the destination before re-sending. Missing or failed receipts do not prove nothing was delivered; running sends may still finish.</p>}
          </> : <p className="gtm-run-meta">Output delivery tracking not recorded.</p>}
          <details><summary>Run details</summary><dl>
            <dt>Run ID</dt><dd>{run.run_id || "Not recorded"}</dd>
            <dt>Started</dt><dd>{stamp(run.started_at)}</dd>
            <dt>Last heartbeat</dt><dd>{stamp(run.last_heartbeat)}</dd>
            <dt>Finished</dt><dd>{stamp(run.completed_at)}</dd>
            {run.status === "pending" && <><dt>Eligible after</dt><dd>{stamp(run.next_run_at)}</dd></>}
          </dl></details>
        </li>)}</ol>
        {query.hasNextPage && <Button className="mt-4" loading={query.isFetchingNextPage} disabled={query.isFetching} onClick={() => void query.fetchNextPage()}>Load older runs</Button>}
      </Dialog.Body>
      <Dialog.Footer><p className="gtm-run-meta">For cell errors, inspect the workbook and review Fill missing. Re-sending output columns needs care.</p><Dialog.Close render={<Button />}>Close</Dialog.Close></Dialog.Footer>
    </Dialog.Popup>
  </Dialog.Root>
}
