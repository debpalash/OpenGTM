import { useEffect, useRef, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Play, Sparkles, Square } from "lucide-react"
import { toast } from "sonner"
import { Button, Dialog } from "@/design-system/primitives"
import { fetchRunEstimate } from "@/lib/workbook-api"
import { useRunWorkbook, useStopWorkbook } from "@/lib/workbook-hooks"
import "./run-review.css"

type Scope = { fillMissing: boolean; viewId: string | null; viewName: string; search: string }

export function WorkbookRunReview({ workbookId, viewId, viewName, search, running, busy, outputColumns }: {
  workbookId: string; viewId: string | null; viewName: string; search: string
  running: boolean; busy: boolean; outputColumns: string[]
}) {
  const [scope, setScope] = useState<Scope | null>(null)
  const [error, setError] = useState("")
  const [needsReview, setNeedsReview] = useState(false)
  const submitting = useRef(false)
  const trigger = useRef<HTMLButtonElement | null>(null)
  const stopButton = useRef<HTMLButtonElement | null>(null)
  const errorMessage = useRef<HTMLParagraphElement | null>(null)
  const run = useRunWorkbook(workbookId)
  const stop = useStopWorkbook(workbookId)
  const estimate = useQuery({
    queryKey: ["workbook-run-review", workbookId, scope?.viewId, scope?.search],
    queryFn: ({ signal }) => fetchRunEstimate(workbookId, scope?.viewId, scope?.search, signal),
    enabled: !!scope, retry: false, staleTime: 0,
  })
  useEffect(() => { if (error) errorMessage.current?.focus() }, [error])

  function open(fillMissing: boolean, button: HTMLButtonElement) {
    trigger.current = button
    setError("")
    setNeedsReview(false)
    setScope({ fillMissing, viewId, viewName, search: search.trim() })
  }
  async function start() {
    if (!scope || !estimate.data || estimate.isFetching || estimate.isError || needsReview || submitting.current || running) return
    submitting.current = true
    setError("")
    try {
      const result = await run.mutateAsync({
        view_id: scope.viewId || undefined, search: scope.search || undefined,
        expected_rows: estimate.data.rows, ...(scope.fillMissing ? { fill_missing: true } : {}),
      })
      toast.success(result.job_id ? `Run #${result.job_id}: ${result.message}` : result.message)
      setScope(null)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not start. Check activity before trying again.")
      setNeedsReview(true)
    } finally { submitting.current = false }
  }

  return <>
    {running ? <Button ref={stopButton} color="danger" loading={stop.isPending} startIcon={<Square aria-hidden="true" className="size-4" />} onClick={() => stop.mutate(undefined, {
      onSuccess: () => toast.success("Stop requested"), onError: () => toast.error("Could not stop the run. Try again."),
    })}>Stop</Button> : <>
      <Button disabled={busy || run.isPending} onClick={event => open(true, event.currentTarget)} startIcon={<Sparkles aria-hidden="true" className="size-4" />}>Fill missing</Button>
      <Button disabled={busy || run.isPending} variant="solid" onClick={event => open(false, event.currentTarget)} startIcon={<Play aria-hidden="true" className="size-4" />}>Review run</Button>
    </>}
    <Dialog.Root open={!!scope} onOpenChange={(open, details) => {
      if (submitting.current) { details.cancel(); return }
      if (!open) setScope(null)
    }}>
      <Dialog.Popup size="lg" className="gtm-run-review" finalFocus={() => trigger.current?.isConnected ? trigger.current : stopButton.current}>
        <Dialog.Header><Dialog.Title>{scope?.fillMissing ? "Review fill missing" : "Review workbook run"}</Dialog.Title><Dialog.Description>Run every matching row across all pages. Checkbox selection does not limit this run.</Dialog.Description>
          {outputColumns.length > 0 && <p className="text-sm font-medium">Includes external sends from {outputColumns.length} output {outputColumns.length === 1 ? "column" : "columns"}.</p>}
        </Dialog.Header>
        <Dialog.Body className="gtm-run-review-body">
          <dl className="gtm-run-facts"><dt>Saved view</dt><dd>{scope?.viewName || "All rows"}</dd><dt>Search</dt><dd>{scope?.search || "None"}</dd><dt>Mode</dt><dd>{scope?.fillMissing ? "Fill incomplete cells; preserve complete values" : "Normal run; existing success-skip rules apply"}</dd></dl>
          {estimate.isFetching ? <p role="status" className="mt-4 text-sm">Loading current estimate…</p> : estimate.isError ? <p role="alert" className="mt-4 text-sm text-destructive">{estimate.error.message}</p> : estimate.data && <>
            <dl className="gtm-run-facts mt-4"><dt>Matching rows</dt><dd>{estimate.data.rows.toLocaleString()}</dd><dt>Catalog provider estimate</dt><dd>${estimate.data.best_usd.toFixed(4)}–${estimate.data.worst_usd.toFixed(4)}</dd></dl>
            <p className="mt-3 text-sm text-muted-foreground">Not a spending cap or a complete quote. AI, research, HTTP, and destination charges may not be included. Fill-missing estimates do not subtract already-complete cells.</p>
            {!!estimate.data.unknown_providers?.length && <p role="alert" className="mt-3 text-sm break-words">Unpriced providers: {estimate.data.unknown_providers.join(", ")}. Their charges are excluded from this estimate. Review provider pricing before starting.</p>}
            {estimate.data.catalog_complete === undefined && <p role="status" className="mt-3 text-sm">Provider price coverage was not reported. Do not interpret a zero estimate as a free run.</p>}
            {estimate.data.rows === 0 && <p role="status" className="mt-3 text-sm">No stored rows match this scope. Legacy lead-backed workbooks must be migrated before using this review.</p>}
          </>}
          {outputColumns.length > 0 && <p className="mt-4 text-sm">External actions: {outputColumns.join(", ")}. Eligible rows may be sent to these columns’ configured destinations. This is not a forced re-send.</p>}
          <p className="mt-3 text-xs text-muted-foreground">Rows are resolved when the run starts. A changed row count requires another review; this is not a frozen record snapshot.</p>
          {error && <p ref={errorMessage} tabIndex={-1} role="alert" className="mt-3 text-sm text-destructive">{error}</p>}
          {(estimate.isError || needsReview) && <Button className="mt-3" disabled={estimate.isFetching} onClick={async () => {
            const result = await estimate.refetch()
            if (!result.isError) { setNeedsReview(false); setError("") }
          }}>Refresh review</Button>}
        </Dialog.Body>
        <Dialog.Footer><Dialog.Close render={<Button disabled={run.isPending} />}>Cancel</Dialog.Close><Button variant="solid" loading={run.isPending} disabled={busy || running || estimate.isFetching || estimate.isError || !estimate.data?.rows || needsReview} onClick={() => void start()}>Start run</Button></Dialog.Footer>
      </Dialog.Popup>
    </Dialog.Root>
  </>
}
