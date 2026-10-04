// Plugins: the Go plugin platform's catalog and recent runs.
//
// Data comes from /api/v2 (served by the Go server). On the legacy stack those
// routes 404, and the page shows how to start the Go server instead. Run
// status is live through /api/v2/events, with polling while a run is active
// and the stream is down.

import { useMemo, useState } from "react"
import { getRouteApi, useNavigate } from "@tanstack/react-router"
import { toast } from "sonner"
import { Puzzle } from "lucide-react"
import { PlatformUnavailable } from "@/components/plugins/platform-unavailable"
import { PluginCatalogView } from "@/components/plugins/plugin-catalog"
import { PluginRunsTable } from "@/components/plugins/plugin-runs-table"
import { RunPluginDialog } from "@/components/plugins/run-plugin-dialog"
import { StreamIndicator } from "@/components/plugins/stream-indicator"
import { ErrorState, Loading } from "@/components/states"
import { Card } from "@/components/ui/card"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { isActiveRun, isPlatformUnavailable, type PluginInfo, type PluginRun } from "@/lib/plugin-api"
import { pluginTitle } from "@/lib/plugin-catalog"
import { usePlatformEvents, usePluginCatalog, usePluginRuns } from "@/lib/plugin-hooks"

const route = getRouteApi("/_app/plugins")

function CatalogSkeleton() {
  return <div role="status" aria-label="Loading plugins" className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
    {Array.from({ length: 6 }, (_, i) => <Card key={i} className="gap-3 p-4">
      <div className="flex items-center gap-3"><Skeleton className="size-8 rounded-md" /><div className="grid flex-1 gap-1.5"><Skeleton className="h-3.5 w-1/2" /><Skeleton className="h-3 w-1/3" /></div></div>
      <div className="flex gap-1"><Skeleton className="h-5 w-16" /><Skeleton className="h-5 w-20" /></div>
      <Skeleton className="h-3 w-full" /><Skeleton className="h-3 w-4/5" />
      <Skeleton className="h-14 w-full" />
    </Card>)}
  </div>
}

export default function PluginsPage() {
  const search = route.useSearch()
  const navigate = useNavigate()
  const tab = search.tab === "runs" ? "runs" : "catalog"
  const catalog = usePluginCatalog()
  const stream = usePlatformEvents(catalog.isSuccess)
  const runs = usePluginRuns(stream === "live", !isPlatformUnavailable(catalog.error))
  const [runTarget, setRunTarget] = useState<PluginInfo | null>(null)
  const [highlight, setHighlight] = useState<string | null>(null)
  const pluginsByName = useMemo(() => new Map((catalog.data?.plugins ?? []).map(p => [p.name, p])), [catalog.data])
  const activeRuns = runs.data?.filter(isActiveRun).length ?? 0

  const setTab = (value: string) => void navigate({ to: "/plugins", search: value === "runs" ? { tab: "runs" } : {}, replace: true })

  function onCreated(run: PluginRun) {
    setHighlight(run.id)
    setTab("runs")
    const plugin = pluginsByName.get(run.plugin)
    toast.success(`${plugin ? pluginTitle(plugin) : run.plugin} run queued`, {
      action: { label: "Open", onClick: () => void navigate({ to: "/plugins/runs/$runId", params: { runId: run.id } }) },
    })
  }

  const unavailable = isPlatformUnavailable(catalog.error) || isPlatformUnavailable(runs.error)

  return (
    <div className="flex flex-col gap-3 p-4">
      <header className="flex items-start justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-base font-semibold"><Puzzle aria-hidden="true" className="size-4" />Plugins</h1>
          <p className="text-xs text-muted-foreground">Scrapers, providers and tools on the plugin host. Every record keeps the evidence it came from.</p>
        </div>
        {!unavailable && catalog.isSuccess && <StreamIndicator state={stream} />}
      </header>
      <Separator />
      {unavailable ? <PlatformUnavailable /> : (
        <Tabs value={tab} onValueChange={value => setTab(String(value))}>
          <TabsList aria-label="Plugin sections">
            <TabsTrigger value="catalog">Catalog{catalog.data && <span className="text-[11px] tabular-nums text-muted-foreground">{catalog.data.plugins.length}</span>}</TabsTrigger>
            <TabsTrigger value="runs">Runs{activeRuns > 0 && <span className="rounded-full bg-sky-500/15 px-1.5 text-[11px] tabular-nums text-sky-700 dark:text-sky-300" aria-label={`${activeRuns} active`}>{activeRuns}</span>}</TabsTrigger>
          </TabsList>
          <TabsContent value="catalog" className="pt-2">
            {catalog.isPending ? <CatalogSkeleton />
              : catalog.isError ? <ErrorState error={catalog.error} onRetry={() => void catalog.refetch()} />
              : <PluginCatalogView catalog={catalog.data} onRun={setRunTarget} />}
          </TabsContent>
          <TabsContent value="runs" className="pt-2">
            {runs.isPending ? <Loading rows={5} />
              : runs.isError ? <ErrorState error={runs.error} onRetry={() => void runs.refetch()} />
              : <PluginRunsTable runs={runs.data} plugins={pluginsByName} highlightId={highlight} />}
          </TabsContent>
        </Tabs>
      )}
      <RunPluginDialog plugin={runTarget} onOpenChange={open => { if (!open) setRunTarget(null) }} onCreated={onCreated} />
    </div>
  )
}
