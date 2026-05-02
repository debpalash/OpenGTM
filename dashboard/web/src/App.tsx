import { useCallback, useEffect, useRef, useState } from "react"
import { Toaster, toast } from "sonner"
import { Badge } from "@/components/ui/badge"
import { TooltipProvider } from "@/components/ui/tooltip"
import { Button } from "@/components/ui/button"
import { Separator } from "@/components/ui/separator"
import { Kbd } from "@/components/ui/kbd"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination"
import { StatsStrip } from "@/components/stats-strip"
import { LeadsTable } from "@/components/leads-table"
import { LeadsFilters, type FilterState } from "@/components/leads-filters"
import { LeadDetailSheet } from "@/components/lead-detail-sheet"
import { AddLeadDialog } from "@/components/add-lead-dialog"
import { fetchLeads, fetchStats, fetchFilters, exportCSVUrl, type Lead, type Stats, type Filters } from "@/lib/api"

const PAGE_SIZE = 100

export default function App() {
  const [leads, setLeads] = useState<Lead[]>([])
  const [stats, setStats] = useState<Stats | null>(null)
  const [filters, setFilters] = useState<Filters | null>(null)
  const [filterState, setFilterState] = useState<FilterState>({
    search: "", city: "", tier: "", status: "", source: "", orderBy: "score DESC",
  })
  const [page, setPage] = useState(0)
  const [selectedLead, setSelectedLead] = useState<Lead | null>(null)
  const [sheetOpen, setSheetOpen] = useState(false)
  const [addOpen, setAddOpen] = useState(false)
  const searchRef = useRef<HTMLInputElement>(null)

  const loadLeads = useCallback(async () => {
    const params: Record<string, string> = {
      limit: String(PAGE_SIZE),
      offset: String(page * PAGE_SIZE),
      order_by: filterState.orderBy,
    }
    if (filterState.search) params.search = filterState.search
    if (filterState.city) params.city = filterState.city
    if (filterState.tier) params.tier = filterState.tier
    if (filterState.status) params.status = filterState.status
    if (filterState.source) params.source = filterState.source
    const data = await fetchLeads(params)
    setLeads(data)
  }, [page, filterState])

  const loadStats = useCallback(async () => {
    const data = await fetchStats()
    setStats(data)
  }, [])

  const loadFilters = useCallback(async () => {
    const data = await fetchFilters()
    setFilters(data)
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

  const handleRowClick = (lead: Lead) => {
    setSelectedLead(lead)
    setSheetOpen(true)
  }

  const handleFilterChange = (f: FilterState) => {
    setFilterState(f)
    setPage(0)
  }

  const handleExport = () => {
    const params: Record<string, string> = {}
    if (filterState.tier) params.tier = filterState.tier
    if (filterState.status) params.status = filterState.status
    if (filterState.city) params.city = filterState.city
    window.location.href = exportCSVUrl(params)
    toast.success("Exporting CSV...")
  }

  const handleAdded = () => {
    setAddOpen(false)
    refresh()
    toast.success("Lead added")
  }

  const handleStatusChange = () => {
    refresh()
    toast.success("Status updated")
  }

  const handleDelete = () => {
    setSheetOpen(false)
    refresh()
    toast.success("Lead deleted")
  }

  return (
    <TooltipProvider>
      <div className="h-screen flex flex-col bg-zinc-950 text-zinc-100">
        {/* ── Header ── */}
        <div className="flex items-center justify-between px-4 h-10 border-b border-zinc-800 shrink-0">
          <div className="flex items-center gap-2">
            <Badge variant="outline" className="text-xs font-bold border-zinc-700 bg-zinc-900">⚡ Yupcha Leads</Badge>
          </div>
          <div className="flex items-center gap-1.5">
            <LeadsFilters
              filters={filters}
              state={filterState}
              onChange={handleFilterChange}
              searchRef={searchRef}
            />
            <Separator orientation="vertical" className="h-4 mx-1" />
            <Button variant="ghost" size="sm" className="h-7 text-xs">
              <span onClick={handleExport}>Export</span>
            </Button>
            <Button size="sm" className="h-7 text-xs" onClick={() => setAddOpen(true)}>
              + Add
            </Button>
          </div>
        </div>

        {/* ── Stats Strip ── */}
        {stats && <StatsStrip stats={stats} />}

        {/* ── Table ── */}
        <ScrollArea className="flex-1">
          <LeadsTable leads={leads} onRowClick={handleRowClick} onStatusChange={handleStatusChange} />
        </ScrollArea>

        {/* ── Footer ── */}
        <div className="flex items-center justify-between px-4 h-8 border-t border-zinc-800 shrink-0">
          <Badge variant="secondary" className="text-[10px] h-5 bg-zinc-900 text-zinc-400">
            {leads.length} leads · Page {page + 1}
          </Badge>
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
          <div className="flex items-center gap-1 text-[10px] text-zinc-600">
            <Kbd>/</Kbd> search
            <Separator orientation="vertical" className="h-3 mx-1" />
            <Kbd>esc</Kbd> close
          </div>
        </div>

        {/* ── Panels ── */}
        <LeadDetailSheet
          lead={selectedLead}
          open={sheetOpen}
          onOpenChange={setSheetOpen}
          onStatusChange={handleStatusChange}
          onDelete={handleDelete}
        />
        <AddLeadDialog open={addOpen} onOpenChange={setAddOpen} onAdded={handleAdded} />
        <Toaster position="bottom-right" theme="dark" />
      </div>
    </TooltipProvider>
  )
}
