// One plugin run: status, stats, inputs, errors, cancel, and the results
// table with per-record evidence.

import { useEffect, useState, type ReactNode } from "react"
import { Link, getRouteApi } from "@tanstack/react-router"
import { toast } from "sonner"
import { ArrowLeft, Ban, Hourglass, Inbox, SearchX, TriangleAlert } from "lucide-react"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { Gate } from "@/components/gate"
import { PlatformUnavailable } from "@/components/plugins/platform-unavailable"
import { PluginResultsTable } from "@/components/plugins/plugin-results-table"
import { RunStatusBadge } from "@/components/plugins/run-status-badge"
import { StreamIndicator } from "@/components/plugins/stream-indicator"
import { Empty, ErrorState, Loading } from "@/components/states"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { ApiError } from "@/lib/api"
import { apiErrorMessage } from "@/lib/automation-hooks"
import { isActiveRun, isPlatformUnavailable, type PluginRun } from "@/lib/plugin-api"
import { pluginTitle } from "@/lib/plugin-catalog"
import { absoluteTime, cellText, formatCost, formatDuration, relativeTime, runDurationMs } from "@/lib/plugin-format"
import { useCancelPluginRun, usePlatformEvents, usePluginCatalog, usePluginRun, usePluginRunResults } from "@/lib/plugin-hooks"

const route = getRouteApi("/_app/plugins/runs/$runId")

function useNow(active: boolean) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [active])
  return now
}

function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return <Card className="gap-1 p-3">
    <p className="text-[10px] tracking-wide text-muted-foreground uppercase">{label}</p>
    <p className="text-xl font-semibold tabular-nums">{value}</p>
    {hint && <p className="text-[11px] text-muted-foreground">{hint}</p>}
  </Card>
}

function Meta({ label, children }: { label: string; children: ReactNode }) {
  return <div className="grid min-w-0 gap-0.5">
    <dt className="text-[11px] text-muted-foreground">{label}</dt>
    <dd className="min-w-0 truncate text-xs">{children}</dd>
  </div>
}

function When({ iso }: { iso: string | null }) {
  if (!iso) return <span className="text-muted-foreground">—</span>
  return <time dateTime={iso} title={absoluteTime(iso)}>{relativeTime(iso)}</time>
}

function RunResults({ run }: { run: PluginRun }) {
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(100)
  const finished = !isActiveRun(run)
  const results = usePluginRunResults(run.id, offset, limit, finished)

  if (!finished) {
    const records = run.stats?.records
    return <Empty icon={Hourglass} title={run.status === "pending" ? "Waiting for a worker" : "Running"}
      description={`Results are saved when the run completes${records ? `; ${records.toLocaleString()} records extracted so far` : ""}.`} />
  }
  if (results.isPending) return <Loading rows={6} />
  if (results.isError) return <ErrorState error={results.error} onRetry={() => void results.refetch()} />
  if (results.data.total === 0) {
    return <Empty icon={Inbox} title="No records"
      description={run.status === "completed" ? run.stats?.provider_error ? `The provider returned no data: ${run.stats.provider_error}.` : "The run finished without producing records." : "The run ended before producing records."} />
  }
  return <PluginResultsTable results={results.data.results} total={results.data.total} offset={offset} limit={limit}
    loading={results.isFetching} onPage={setOffset} onLimit={value => { setLimit(value); setOffset(0) }} />
}

function RunHeaderSkeleton() {
  return <div role="status" aria-label="Loading run" className="grid gap-3">
    <Skeleton className="h-5 w-64" />
    <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">{Array.from({ length: 4 }, (_, i) => <Skeleton key={i} className="h-20" />)}</div>
    <Skeleton className="h-16" />
  </div>
}

export default function PluginRunPage() {
  const { runId } = route.useParams()
  const catalog = usePluginCatalog()
  const unavailableCatalog = isPlatformUnavailable(catalog.error)
  const stream = usePlatformEvents(catalog.isSuccess)
  const query = usePluginRun(runId, stream === "live", !unavailableCatalog)
  const cancel = useCancelPluginRun()
  const [confirming, setConfirming] = useState(false)
  const run = query.data
  const now = useNow(!!run && run.status === "running")
  const plugin = run ? catalog.data?.plugins.find(p => p.name === run.plugin) : undefined

  function confirmCancel() {
    cancel.mutate(runId, {
      onSuccess: () => { setConfirming(false); toast.success("Run cancelled") },
      onError: error => {
        setConfirming(false)
        if (error instanceof ApiError && error.status === 409) toast.info("The run already finished, so there was nothing to cancel.")
        else if (error instanceof ApiError && error.status === 403) toast.error("Your workspace role cannot cancel plugin runs.")
        else toast.error(apiErrorMessage(error))
      },
    })
  }

  const back = <Link to="/plugins" search={{ tab: "runs" }} className="inline-flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground">
    <ArrowLeft aria-hidden="true" className="size-3" />Plugin runs
  </Link>

  if (unavailableCatalog || isPlatformUnavailable(query.error)) return <div className="flex flex-col gap-3 p-4">{back}<PlatformUnavailable /></div>
  if (query.isError) {
    const missing = query.error instanceof ApiError && query.error.status === 404
    return <div className="flex flex-col gap-3 p-4">{back}
      {missing ? <Empty icon={SearchX} title="Run not found" description="It may belong to another workspace, or the ID is wrong." />
        : <ErrorState error={query.error} onRetry={() => void query.refetch()} />}
    </div>
  }
  if (!run) return <div className="flex flex-col gap-3 p-4">{back}<RunHeaderSkeleton /></div>

  const stats = run.stats ?? {}
  const active = isActiveRun(run)
  const inputs = Object.entries(run.inputs ?? {})

  return (
    <div className="flex flex-col gap-4 p-4">
      {back}
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-base font-semibold">{plugin ? pluginTitle(plugin) : run.plugin}</h1>
            <RunStatusBadge status={run.status} />
          </div>
          <p className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground">{run.plugin}{run.plugin_version && ` · v${run.plugin_version}`} · run {run.id}</p>
        </div>
        <div className="flex items-center gap-3">
          {catalog.isSuccess && active && <StreamIndicator state={stream} />}
          {active && <Gate need="editor">
            <Button variant="outline" size="sm" className="gap-1" onClick={() => setConfirming(true)} disabled={cancel.isPending}>
              <Ban aria-hidden="true" />Cancel run
            </Button>
          </Gate>}
        </div>
      </header>

      {run.status === "failed" && run.error && <Alert variant="destructive"><TriangleAlert aria-hidden="true" /><AlertTitle>Run failed</AlertTitle><AlertDescription className="break-words">{run.error}</AlertDescription></Alert>}
      {run.status === "cancelled" && <Alert><Ban aria-hidden="true" /><AlertTitle>Cancelled</AlertTitle><AlertDescription>{run.error || "Cancelled by user"}</AlertDescription></Alert>}
      {active && run.error && <Alert><TriangleAlert aria-hidden="true" /><AlertTitle>Retrying</AlertTitle><AlertDescription className="break-words">{run.error}</AlertDescription></Alert>}
      {stats.provider_error && <Alert><TriangleAlert aria-hidden="true" /><AlertTitle>Provider error</AlertTitle><AlertDescription className="break-words">{stats.provider_error}</AlertDescription></Alert>}
      {stats.stopped && <Alert><TriangleAlert aria-hidden="true" /><AlertTitle>Stopped early</AlertTitle><AlertDescription className="break-words">{stats.stopped}</AlertDescription></Alert>}

      <section aria-label="Run statistics" className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Records" value={stats.records != null ? stats.records.toLocaleString() : "—"} hint={active && stats.records != null ? "So far" : undefined} />
        <Stat label="Pages" value={stats.pages != null ? stats.pages.toLocaleString() : "—"} />
        <Stat label="Duration" value={formatDuration(runDurationMs(run, now))} hint={run.status === "running" ? "Elapsed" : undefined} />
        <Stat label="Cost" value={formatCost(stats.cost_usd)} />
      </section>

      <Card className="gap-3 p-4">
        <dl className="grid gap-3 sm:grid-cols-3 lg:grid-cols-6">
          <Meta label="Created by">{run.created_by || "—"}</Meta>
          <Meta label="Created"><When iso={run.created_at} /></Meta>
          <Meta label="Started"><When iso={run.started_at} /></Meta>
          <Meta label="Completed"><When iso={run.completed_at} /></Meta>
          <Meta label="Job">{run.job_id ?? "—"}</Meta>
          <Meta label="Run ID"><span className="font-mono" title={run.id}>{run.id}</span></Meta>
        </dl>
        <Separator />
        <div className="grid gap-1.5">
          <h2 className="text-xs font-medium">Inputs</h2>
          {inputs.length === 0 ? <p className="text-xs text-muted-foreground">No inputs.</p>
            : <dl className="grid gap-1">{inputs.map(([key, value]) => <div key={key} className="grid grid-cols-[minmax(6rem,12rem)_minmax(0,1fr)] gap-2 text-xs">
              <dt className="truncate font-mono text-muted-foreground">{key}</dt><dd className="font-mono break-all">{cellText(value) || "—"}</dd>
            </div>)}</dl>}
        </div>
      </Card>

      <section aria-labelledby="run-results-heading" className="grid gap-2">
        <h2 id="run-results-heading" className="text-sm font-semibold">Results</h2>
        <RunResults key={`${run.id}:${active}`} run={run} />
      </section>

      <ConfirmDialog open={confirming} onOpenChange={setConfirming} title="Cancel this run?" destructive pending={cancel.isPending}
        confirmLabel="Cancel run" cancelLabel="Keep running" onConfirm={confirmCancel}
        description="The worker stops at the next checkpoint. Records not yet saved are discarded." />
    </div>
  )
}
