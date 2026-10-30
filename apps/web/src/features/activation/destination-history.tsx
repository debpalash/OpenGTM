import { useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { getActiveWorkspace } from "@/lib/auth"
import { relativeTime } from "@/lib/record-overview"
import { Button } from "@/components/ui/button"
import { NativeSelect } from "@/components/ui/native-select"
import {
  ACTIVE_RUNS,
  fetchDeliveryPage,
  fetchRunPage,
  type DeliveryReceipt
} from "./destination-client"
import type { DestinationRun } from "@/lib/api"

export const runHistoryKey = (workspace: string | null, id: string) => [
  "destination-run-history",
  workspace,
  id
]
export const runLabel = (status: string) =>
  ({
    pending: "Queued",
    running: "Sending",
    cancelling: "Cancellation requested",
    completed: "Completed",
    completed_with_errors: "Finished with errors",
    failed: "Failed",
    cancelled: "Cancelled"
  })[status] || "Status unavailable"
export function RunSummary({ run }: { run: DestinationRun }) {
  const detail = {
    pending:
      run.attempted || run.succeeded || run.failed || run.skipped
        ? "Queued for another attempt. Counts include earlier attempts of this run."
        : "Delivery is not confirmed yet.",
    running: "Deliveries are in progress.",
    cancelling: "In-flight deliveries may still finish.",
    cancelled: "Completed deliveries are not undone.",
    failed: "Some deliveries may already have succeeded.",
    completed_with_errors: "Review failed deliveries before retrying.",
    completed: "Review the receipt counts below."
  }[run.status]
  return (
    <section
      className="destination-run-summary"
      aria-label="Selected run receipt"
    >
      <div className="destination-run-heading">
        <strong>{runLabel(run.status)}</strong>
        <time title={run.created_at + " UTC"}>
          {relativeTime(run.created_at)}
        </time>
      </div>
      <p role="status">
        <span className="sr-only">{runLabel(run.status)}. </span>
        {detail}
      </p>
      <code className="destination-id">Run {run.id}</code>
      <dl className="destination-counts">
        {[
          ["Attempted", run.attempted],
          ["Succeeded", run.succeeded],
          ["Failed", run.failed],
          ["Skipped", run.skipped]
        ].map(([name, value]) => (
          <div key={name}>
            <dt>{name}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      {run.error && <p className="destination-error">{run.error}</p>}
    </section>
  )
}
function subject(row: DeliveryReceipt) {
  if (row.person_id)
    return {
      label: "Person record",
      href: `/people?record=${encodeURIComponent(row.person_id)}`
    }
  if (row.company_id)
    return {
      label: "Company record",
      href: `/companies?record=${encodeURIComponent(row.company_id)}`
    }
  if (row.lead_id != null && row.lead_id > 0)
    return {
      label: `Legacy lead ${row.lead_id}`,
      href: `/leads/${row.lead_id}`
    }
  return { label: "Record unavailable", href: null }
}
function DeliveryLedger({
  destinationID,
  run
}: {
  destinationID: string
  run: DestinationRun
}) {
  const [status, setStatus] = useState("")
  const query = useInfiniteQuery({
    queryKey: [
      "destination-deliveries",
      getActiveWorkspace(),
      destinationID,
      run.id,
      status,
      run.status
    ],
    queryFn: ({ pageParam, signal }) =>
      fetchDeliveryPage(destinationID, run.id, pageParam, status, signal),
    initialPageParam: null as number | null,
    getNextPageParam: (page) =>
      page.length === 50 ? page.at(-1)!.id : undefined,
    refetchInterval: ACTIVE_RUNS.has(run.status) ? 2000 : false
  })
  const rows = [
    ...new Map(query.data?.pages.flat().map((row) => [row.id, row])).values()
  ]
  return (
    <section aria-label="Run deliveries" className="destination-ledger">
      <div className="destination-ledger-toolbar">
        <h3>Deliveries</h3>
        <NativeSelect
          aria-label="Delivery status"
          value={status}
          onChange={(event) => setStatus(event.target.value)}
        >
          <option value="">All statuses</option>
          <option value="failed">Failed</option>
          <option value="success">Succeeded</option>
          <option value="pending">Pending</option>
          <option value="in_flight">In flight</option>
          <option value="cancelled">Cancelled</option>
        </NativeSelect>
      </div>
      {query.isPending && <p role="status">Loading deliveries…</p>}
      {query.isError && (
        <div role="alert" className="destination-error-block">
          <p>
            Deliveries could not be loaded.{" "}
            {rows.length ? "Showing the last successful page." : ""}
          </p>
          <Button
            size="sm"
            variant="outline"
            disabled={query.isFetching}
            onClick={() =>
              void (query.isFetchNextPageError
                ? query.fetchNextPage()
                : query.refetch())
            }
          >
            Try deliveries again
          </Button>
        </div>
      )}
      {!query.isPending && !query.isError && !rows.length && (
        <p className="destination-muted">
          {status
            ? "No deliveries with this status on the loaded run."
            : "No delivery receipts recorded yet. A queued or running job may still create them."}
        </p>
      )}
      <ul className="destination-deliveries">
        {rows.map((row) => {
          const record = subject(row)
          return (
            <li key={row.id}>
              <div className="destination-delivery-heading">
                {record.href ? (
                  <Link to={record.href}>{record.label}</Link>
                ) : (
                  <span>{record.label}</span>
                )}
                <span>
                  {row.status === "success"
                    ? "Succeeded"
                    : row.status.replaceAll("_", " ")}
                </span>
              </div>
              <p>
                {row.operation} · {row.attempts} attempt
                {row.attempts === 1 ? "" : "s"}
                {row.external_id ? ` · External ID: ${row.external_id}` : ""}
              </p>
              {row.summary && <p>{row.summary}</p>}
              {row.error && <p className="destination-error">{row.error}</p>}
            </li>
          )
        })}
      </ul>
      {query.hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          disabled={query.isFetching}
          onClick={() => void query.fetchNextPage()}
        >
          Load more deliveries
        </Button>
      )}
      {!!rows.length && (
        <p className="destination-muted">
          {rows.length} delivery receipts loaded. Counts above apply to the run.
        </p>
      )}
    </section>
  )
}
export function DestinationHistory({
  destinationID,
  receipt,
  selectedID,
  onSelect
}: {
  destinationID: string
  receipt: DestinationRun | null
  selectedID: string | null
  onSelect: (id: string) => void
}) {
  const query = useInfiniteQuery({
    queryKey: runHistoryKey(getActiveWorkspace(), destinationID),
    queryFn: ({ pageParam, signal }) =>
      fetchRunPage(destinationID, pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: (page) => page.next_cursor || undefined,
    refetchInterval: 3000
  })
  const runs = [
    ...new Map(
      query.data?.pages.flatMap((page) => page.runs).map((run) => [run.id, run])
    ).values()
  ]
  const selected =
    runs.find((run) => run.id === selectedID) ||
    (receipt?.id === selectedID ? receipt : null) ||
    (!selectedID ? runs[0] : null)
  return (
    <div className="destination-history">
      {query.isError && (
        <div role="alert" className="destination-error-block">
          <p>
            Run history could not be loaded.{" "}
            {receipt
              ? "The confirmed action receipt is kept below."
              : runs.length
                ? "Showing the last successful history."
                : ""}
          </p>
          <Button
            size="sm"
            variant="outline"
            disabled={query.isFetching}
            onClick={() =>
              void (query.isFetchNextPageError
                ? query.fetchNextPage()
                : query.refetch())
            }
          >
            Try run history again
          </Button>
        </div>
      )}
      {query.isPending && <p role="status">Loading run history…</p>}
      {runs.length > 0 && (
        <div
          className="destination-run-list"
          role="group"
          aria-label="Choose run"
        >
          {runs.map((run) => (
            <button
              key={run.id}
              type="button"
              aria-pressed={selected?.id === run.id}
              onClick={() => onSelect(run.id)}
            >
              <span>{runLabel(run.status)}</span>
              <code>{run.id.slice(0, 8)}</code>
              <span>{relativeTime(run.created_at)}</span>
            </button>
          ))}
        </div>
      )}
      {query.hasNextPage && (
        <Button
          size="sm"
          variant="outline"
          disabled={query.isFetching}
          onClick={() => void query.fetchNextPage()}
        >
          Load older runs
        </Button>
      )}
      {!query.isPending && !query.isError && !runs.length && !receipt && (
        <p className="destination-muted">
          No sync runs have been recorded for this destination.
        </p>
      )}
      {selected && (
        <>
          <RunSummary run={selected} />
          <DeliveryLedger
            key={selected.id}
            destinationID={destinationID}
            run={selected}
          />
        </>
      )}
    </div>
  )
}
