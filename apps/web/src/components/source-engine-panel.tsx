/**
 * Source Engine panel (Workbook v2) — surfaces the live source engine in one place:
 *   • Source   — add an ICP source column and run it (live row sourcing)
 *   • Cost     — budget ceiling + spend-to-date (Pillar 2)
 *   • Living   — refresh cadence + signal triggers + "refresh now" (Pillar 3)
 *   • Entities — resolved companies with cross-source corroboration (Pillar 1)
 *   • Activity — the living-workbook event feed
 *
 * Self-contained: uses the workbook-api client + shadcn primitives only.
 */

import { useEffect, useState } from "react"
import { toast } from "sonner"
import {
  Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription,
} from "@/components/ui/sheet"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import { Radar, Play, DollarSign, RefreshCw, Network, Activity, Loader2 } from "lucide-react"
import {
  addSourceColumn, runSourceColumn, fetchWorkbookCost, setWorkbookBudget,
  setRefreshPolicy, refreshWorkbook, fetchActivity, fetchEntities,
  type CostInfo, type ActivityItem, type CompanyEntity, type RefreshPolicy,
} from "@/lib/workbook-api"

export function SourceEnginePanel({
  workbookId, open, onOpenChange, onChanged,
}: {
  workbookId: string
  open: boolean
  onOpenChange: (v: boolean) => void
  onChanged?: () => void
}) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-[440px] sm:max-w-[440px] overflow-y-auto">
        <SheetHeader>
          <SheetTitle className="flex items-center gap-2">
            <Radar className="h-4 w-4" /> Source Engine
          </SheetTitle>
          <SheetDescription>Source, enrich, and keep this workbook alive.</SheetDescription>
        </SheetHeader>

        <Tabs defaultValue="source" className="mt-4">
          <TabsList className="grid grid-cols-5 w-full">
            <TabsTrigger value="source"><Radar className="h-3.5 w-3.5" /></TabsTrigger>
            <TabsTrigger value="cost"><DollarSign className="h-3.5 w-3.5" /></TabsTrigger>
            <TabsTrigger value="living"><RefreshCw className="h-3.5 w-3.5" /></TabsTrigger>
            <TabsTrigger value="entities"><Network className="h-3.5 w-3.5" /></TabsTrigger>
            <TabsTrigger value="activity"><Activity className="h-3.5 w-3.5" /></TabsTrigger>
          </TabsList>

          <TabsContent value="source"><SourceTab workbookId={workbookId} onChanged={onChanged} /></TabsContent>
          <TabsContent value="cost"><CostTab workbookId={workbookId} /></TabsContent>
          <TabsContent value="living"><LivingTab workbookId={workbookId} /></TabsContent>
          <TabsContent value="entities"><EntitiesTab workbookId={workbookId} open={open} /></TabsContent>
          <TabsContent value="activity"><ActivityTab workbookId={workbookId} open={open} /></TabsContent>
        </Tabs>
      </SheetContent>
    </Sheet>
  )
}

function SourceTab({ workbookId, onChanged }: { workbookId: string; onChanged?: () => void }) {
  const [icp, setIcp] = useState("")
  const [targetRows, setTargetRows] = useState(50)
  const [busy, setBusy] = useState(false)

  async function run() {
    if (!icp.trim()) { toast.error("Describe who to source"); return }
    setBusy(true)
    try {
      const { column } = await addSourceColumn(workbookId, {
        name: "Source", icp: { description: icp }, target_rows: targetRows,
      })
      await runSourceColumn(workbookId, column.id)
      toast.success("Sourcing started — rows will stream in")
      onChanged?.()
    } catch (e: any) {
      toast.error(e.message || "Failed to start sourcing")
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-3 py-3">
      <label className="text-xs font-medium text-muted-foreground">Ideal customer (ICP)</label>
      <Input
        placeholder="IT staffing companies in Pune, 50–500 employees"
        value={icp} onChange={(e) => setIcp(e.target.value)}
      />
      <div className="flex items-center gap-2">
        <label className="text-xs text-muted-foreground">Max rows</label>
        <Input type="number" className="w-24 h-8" value={targetRows}
          onChange={(e) => setTargetRows(Number(e.target.value) || 0)} />
      </div>
      <Button onClick={run} disabled={busy} className="w-full">
        {busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
        Source leads
      </Button>
      <p className="text-xs text-muted-foreground">
        Fans out across the multi-source engine, entity-resolves results, and appends new rows live.
      </p>
    </div>
  )
}

function CostTab({ workbookId }: { workbookId: string }) {
  const [cost, setCost] = useState<CostInfo | null>(null)
  const [cap, setCap] = useState("0")
  const load = () => fetchWorkbookCost(workbookId).then(setCost).catch(() => {})
  useEffect(() => { load() }, [workbookId])

  async function save() {
    try { setCost(await setWorkbookBudget(workbookId, Number(cap) || 0)); toast.success("Budget updated") }
    catch (e: any) { toast.error(e.message) }
  }
  return (
    <div className="space-y-3 py-3">
      {cost && (
        <div className="rounded-md border p-3 text-sm space-y-1">
          <div className="flex justify-between"><span className="text-muted-foreground">Spent</span><span>${cost.budget_spent_usd.toFixed(3)}</span></div>
          <div className="flex justify-between"><span className="text-muted-foreground">Ceiling</span><span>{cost.unlimited ? "unlimited" : `$${cost.budget_max_usd.toFixed(2)}`}</span></div>
          {!cost.unlimited && <div className="flex justify-between"><span className="text-muted-foreground">Remaining</span><span>${(cost.remaining_usd ?? 0).toFixed(3)}</span></div>}
        </div>
      )}
      <label className="text-xs text-muted-foreground">Spend ceiling (USD, 0 = unlimited)</label>
      <div className="flex gap-2">
        <Input type="number" step="0.01" value={cap} onChange={(e) => setCap(e.target.value)} />
        <Button variant="outline" onClick={save}>Set</Button>
      </div>
      <p className="text-xs text-muted-foreground">Paid providers are skipped once the ceiling is hit; free providers always run.</p>
    </div>
  )
}

function LivingTab({ workbookId }: { workbookId: string }) {
  const [interval, setInterval] = useState<RefreshPolicy["interval"]>("weekly")
  const [signals, setSignals] = useState<string[]>([])
  const SIGS = ["hiring", "funding", "tech_change", "news"]

  async function save() {
    try {
      await setRefreshPolicy(workbookId, { enabled: true, interval, on_signal: signals })
      toast.success(`Living: refresh ${interval}${signals.length ? ` + on ${signals.join(", ")}` : ""}`)
    } catch (e: any) { toast.error(e.message) }
  }
  async function now() {
    try { await refreshWorkbook(workbookId); toast.success("Refreshing now") } catch (e: any) { toast.error(e.message) }
  }
  return (
    <div className="space-y-3 py-3">
      <label className="text-xs text-muted-foreground">Refresh cadence</label>
      <div className="flex gap-1">
        {(["hourly", "daily", "weekly"] as const).map((i) => (
          <Button key={i} size="sm" variant={interval === i ? "default" : "outline"} onClick={() => setInterval(i)}>{i}</Button>
        ))}
      </div>
      <label className="text-xs text-muted-foreground">Trigger on signals</label>
      <div className="flex flex-wrap gap-1">
        {SIGS.map((s) => (
          <Badge key={s} variant={signals.includes(s) ? "default" : "outline"} className="cursor-pointer"
            onClick={() => setSignals((p) => p.includes(s) ? p.filter((x) => x !== s) : [...p, s])}>{s}</Badge>
        ))}
      </div>
      <div className="flex gap-2 pt-1">
        <Button onClick={save} className="flex-1">Make living</Button>
        <Button variant="outline" onClick={now}><RefreshCw className="h-4 w-4" /></Button>
      </div>
    </div>
  )
}

function EntitiesTab({ workbookId, open }: { workbookId: string; open: boolean }) {
  const [ents, setEnts] = useState<CompanyEntity[]>([])
  useEffect(() => { if (open) fetchEntities({ min_corroboration: 1 }).then((r) => setEnts(r.entities)).catch(() => {}) }, [open, workbookId])
  return (
    <div className="space-y-2 py-3">
      {ents.length === 0 && <p className="text-xs text-muted-foreground">No resolved entities yet. Source some leads first.</p>}
      {ents.map((e) => (
        <div key={e.id} className="rounded-md border p-2 text-sm flex items-center justify-between">
          <div className="min-w-0">
            <div className="truncate font-medium">{e.canonical_name}</div>
            <div className="truncate text-xs text-muted-foreground">{e.primary_domain || "—"}</div>
          </div>
          <Badge variant={e.corroboration_count > 1 ? "default" : "outline"} title="distinct sources corroborating this company">
            {e.corroboration_count}× sources
          </Badge>
        </div>
      ))}
    </div>
  )
}

function ActivityTab({ workbookId, open }: { workbookId: string; open: boolean }) {
  const [items, setItems] = useState<ActivityItem[]>([])
  useEffect(() => { if (open) fetchActivity(workbookId).then((r) => setItems(r.activity)).catch(() => {}) }, [open, workbookId])
  return (
    <div className="space-y-2 py-3">
      {items.length === 0 && <p className="text-xs text-muted-foreground">No activity yet.</p>}
      {items.map((a) => (
        <div key={a.id} className="text-xs border-l-2 pl-2 py-0.5">
          <span className="font-medium">{a.kind}</span> · {a.message}
        </div>
      ))}
    </div>
  )
}
