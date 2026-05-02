import { useCallback, useEffect, useRef, useState } from "react"
import { Toaster, toast } from "sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { Separator } from "@/components/ui/separator"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination"
import { CommandBar } from "@/components/command-bar"
import { MetricCards } from "@/components/metric-cards"
import { LeadsTable } from "@/components/leads-table"
import { LiveFeed } from "@/components/live-feed"
import { WorkspacePanel } from "@/components/workspace-panel"
import { LeadDetailSheet } from "@/components/lead-detail-sheet"
import { AddLeadDialog } from "@/components/add-lead-dialog"
import { fetchLeads, fetchStats, fetchFilters, exportCSVUrl, submitCollect, type Lead, type Stats, type Filters } from "@/lib/api"

const PAGE_SIZE = 100

type View = "leads" | "pipeline"

export default function App() {
  const [leads, setLeads] = useState<Lead[]>([])
  const [stats, setStats] = useState<Stats | null>(null)
  const [filters, setFilters] = useState<Filters | null>(null)
  const [search, setSearch] = useState("")
  const [city, setCity] = useState("")
  const [tier, setTier] = useState("")
  const [status, setStatus] = useState("")
  const [source, setSource] = useState("")
  const [orderBy, setOrderBy] = useState("score DESC")
  const [page, setPage] = useState(0)
  const [selectedLead, setSelectedLead] = useState<Lead | null>(null)
  const [sheetOpen, setSheetOpen] = useState(false)
  const [addOpen, setAddOpen] = useState(false)
  const [view, setView] = useState<View>("leads")
  const [workspaceId, setWorkspaceId] = useState("")
  const searchRef = useRef<HTMLInputElement>(null)

  const loadLeads = useCallback(async () => {
    const params: Record<string, string> = {
      limit: String(PAGE_SIZE),
      offset: String(page * PAGE_SIZE),
      order_by: orderBy,
    }
    if (search) params.search = search
    if (city) params.city = city
    if (tier) params.tier = tier
    if (status) params.status = status
    if (source) params.source = source
    if (workspaceId) params.workspace_id = workspaceId
    const data = await fetchLeads(params)
    setLeads(data)
  }, [page, search, city, tier, status, source, orderBy, workspaceId])

  const loadStats = useCallback(async () => {
    setStats(await fetchStats())
  }, [])

  const loadFilters = useCallback(async () => {
    setFilters(await fetchFilters())
  }, [])

  const refresh = useCallback(() => {
    loadLeads()
    loadStats()
  }, [loadLeads, loadStats])

  useEffect(() => { loadFilters() }, [loadFilters])
  useEffect(() => { refresh() }, [refresh])

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "/" && !e.ctrlKey && !e.metaKey && document.activeElement?.tagName !== "INPUT") {
        e.preventDefault()
        searchRef.current?.focus()
      }
      if (e.key === "Escape") {
        setSheetOpen(false)
        searchRef.current?.blur()
      }
    }
    window.addEventListener("keydown", handler)
    return () => window.removeEventListener("keydown", handler)
  }, [])

  const handleCollect = async (query: string) => {
    const result = await submitCollect(query, workspaceId)
    toast.success(`Collection started: ${result.job_id}`)
    setView("pipeline")
  }

  const handleExport = () => {
    const params: Record<string, string> = {}
    if (tier) params.tier = tier
    if (status) params.status = status
    if (city) params.city = city
    window.location.href = exportCSVUrl(params)
    toast.success("Exporting CSV...")
  }

  return (
    <TooltipProvider>
      <div className="h-screen flex flex-col bg-background text-foreground">
        {/* ── Command Bar ── */}
        <CommandBar
          filters={filters}
          search={search}
          city={city}
          tier={tier}
          status={status}
          source={source}
          orderBy={orderBy}
          onSearchChange={(v) => { setSearch(v); setPage(0) }}
          onCityChange={(v) => { setCity(v); setPage(0) }}
          onTierChange={(v) => { setTier(v); setPage(0) }}
          onStatusChange={(v) => { setStatus(v); setPage(0) }}
          onSourceChange={(v) => { setSource(v); setPage(0) }}
          onOrderByChange={setOrderBy}
          onCollect={handleCollect}
          onExport={handleExport}
          onAdd={() => setAddOpen(true)}
          searchRef={searchRef}
          view={view}
          onViewChange={setView}
        />

        {/* ── Metric Cards ── */}
        {stats && <MetricCards stats={stats} />}

        {/* ── Main Content ── */}
        <div className="flex-1 flex min-h-0">
          {/* ── Workspace Sidebar ── */}
          <div className="w-48 border-r border-border shrink-0 hidden lg:block">
            <WorkspacePanel
              activeId={workspaceId}
              onSelect={(id) => { setWorkspaceId(id); setPage(0) }}
            />
          </div>

          {/* ── Table ── */}
          <div className={`flex-1 flex flex-col min-w-0 ${view === "pipeline" && "hidden lg:flex"}`}>
            <ScrollArea className="flex-1">
              <LeadsTable
                leads={leads}
                onRowClick={(lead) => { setSelectedLead(lead); setSheetOpen(true) }}
                onStatusChange={refresh}
              />
            </ScrollArea>

            {/* ── Footer ── */}
            <div className="flex items-center justify-between px-4 h-9 border-t border-border shrink-0 bg-card/50">
              <span className="text-[11px] text-muted-foreground font-medium tabular-nums">
                {leads.length} leads · Page {page + 1}
                {workspaceId && " · Filtered"}
              </span>
              <Pagination className="w-auto mx-0">
                <PaginationContent className="gap-1">
                  <PaginationItem>
                    <PaginationPrevious
                      className={`h-6 text-[10px] ${page === 0 ? "pointer-events-none opacity-30" : ""}`}
                      onClick={() => page > 0 && setPage(p => p - 1)}
                    />
                  </PaginationItem>
                  <PaginationItem>
                    <PaginationNext
                      className={`h-6 text-[10px] ${leads.length < PAGE_SIZE ? "pointer-events-none opacity-30" : ""}`}
                      onClick={() => leads.length >= PAGE_SIZE && setPage(p => p + 1)}
                    />
                  </PaginationItem>
                </PaginationContent>
              </Pagination>
              <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground/50">
                <kbd className="px-1 py-0.5 rounded bg-secondary text-[9px] font-mono">/</kbd> search
                <Separator orientation="vertical" className="h-3 mx-0.5" />
                <kbd className="px-1 py-0.5 rounded bg-secondary text-[9px] font-mono">esc</kbd> close
              </div>
            </div>
          </div>

          {/* ── Pipeline Feed ── */}
          {view === "pipeline" && (
            <div className="w-full lg:w-[400px] lg:border-l border-border flex flex-col bg-card/30">
              <LiveFeed onRefresh={refresh} workspaceId={workspaceId} />
            </div>
          )}
        </div>

        {/* ── Panels ── */}
        <LeadDetailSheet
          lead={selectedLead}
          open={sheetOpen}
          onOpenChange={setSheetOpen}
          onStatusChange={refresh}
          onDelete={() => { setSheetOpen(false); refresh(); toast.success("Lead deleted") }}
        />
        <AddLeadDialog open={addOpen} onOpenChange={setAddOpen} onAdded={() => { setAddOpen(false); refresh(); toast.success("Lead added") }} />
        <Toaster position="bottom-right" theme="dark" />
      </div>
    </TooltipProvider>
  )
}
