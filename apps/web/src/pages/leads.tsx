import { useState, useMemo, useCallback } from "react"
import { useNavigate } from "react-router-dom"
import { type ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import {
  ArrowUpDown, Mail, Phone, MoreHorizontal,
  Plus, Download, RefreshCw, Globe, Flame, Sun, Snowflake, Upload,
  SlidersHorizontal, Bookmark, X, GitMerge, Loader2,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import {
  Popover, PopoverContent, PopoverTrigger,
} from "@/components/ui/popover"
import {
  Dialog, DialogContent, DialogHeader, DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Badge } from "@/components/ui/badge"
import { DataTable } from "@/components/data-table"
import { useLeads, useStats, useFilters, useUpdateStatus, useUpdateLead, useDeleteLead, useCollect, useImportDataCollector } from "@/lib/hooks"
import {
  exportCSVUrl, fetchSimilarLeads, runDedup, mergeDuplicates,
  type Lead, type SimilarLeads, type DedupResult, type DedupSuggestion,
} from "@/lib/api"
import { EditableCell } from "@/components/editable-cell"

const TIER_COLORS: Record<string, string> = {
  hot:          "text-red-400",
  warm:         "text-amber-400",
  cold:         "text-blue-400",
  unqualified:  "text-zinc-500",
}

const STATUS_DOT: Record<string, string> = {
  new:        "bg-blue-400",
  contacted:  "bg-amber-400",
  qualified:  "bg-emerald-400",
  lost:       "bg-red-400",
}

function ScoreBadge({ score, tier }: { score: number; tier: string }) {
  return (
    <span className={`text-xs font-semibold tabular-nums ${TIER_COLORS[tier] || TIER_COLORS.cold}`}>
      {score}
    </span>
  )
}

// ── Saved segments (named filter sets), persisted in localStorage ──
type LeadFilters = {
  city: string; tier: string; status: string; source: string
  scoreMin: string; scoreMax: string; hasEmail: string; hasPhone: string
}
const EMPTY_FILTERS: LeadFilters = {
  city: "", tier: "", status: "", source: "",
  scoreMin: "", scoreMax: "", hasEmail: "", hasPhone: "",
}
const SEGMENTS_KEY = "yupcha:leadSegments"
function loadSegments(): { name: string; filters: LeadFilters }[] {
  try { return JSON.parse(localStorage.getItem(SEGMENTS_KEY) || "[]") } catch { return [] }
}
function saveSegments(segs: { name: string; filters: LeadFilters }[]) {
  localStorage.setItem(SEGMENTS_KEY, JSON.stringify(segs))
}

export default function LeadsPage() {
  const navigate = useNavigate()
  const [f, setF] = useState<LeadFilters>(EMPTY_FILTERS)
  const setFilter = (k: keyof LeadFilters, v: string) => setF(prev => ({ ...prev, [k]: v }))
  const [collectQuery, setCollectQuery] = useState("")
  const [selectedRows, setSelectedRows] = useState<Lead[]>([])
  const [segments, setSegments] = useState(loadSegments)
  const [similar, setSimilar] = useState<SimilarLeads | null>(null)
  const [similarOpen, setSimilarOpen] = useState(false)

  const openSimilar = useCallback(async (lead: Lead) => {
    setSimilarOpen(true)
    setSimilar(null)
    try { setSimilar(await fetchSimilarLeads(lead.id)) }
    catch { toast.error("Couldn't find similar leads") }
  }, [])

  // ── Dedup ──
  const [dedupOpen, setDedupOpen] = useState(false)
  const [dedupData, setDedupData] = useState<DedupResult | null>(null)
  const [dedupRunning, setDedupRunning] = useState(false)
  const [mergingId, setMergingId] = useState<number | null>(null)

  // Map UI filters → GET /api/leads query params (only non-empty).
  const filters: Record<string, string> = {
    limit: "500",
    order_by: "score DESC",
    ...(f.city && { city: f.city }),
    ...(f.tier && { tier: f.tier }),
    ...(f.status && { status: f.status }),
    ...(f.source && { source: f.source }),
    ...(f.scoreMin && { score_min: f.scoreMin }),
    ...(f.scoreMax && { score_max: f.scoreMax }),
    ...(f.hasEmail && { has_email: f.hasEmail }),   // "true" | "false"
    ...(f.hasPhone && { has_phone: f.hasPhone }),
  }
  const activeFilterCount = Object.values(f).filter(Boolean).length

  const { data: leads, isLoading, refetch } = useLeads(filters)
  const { data: stats } = useStats()
  const { data: filterOptions } = useFilters()
  const updateStatusMut = useUpdateStatus()
  const updateLeadMut = useUpdateLead()
  const deleteLeadMut = useDeleteLead()
  const collect = useCollect()
  const importBR = useImportDataCollector()

  const columns: ColumnDef<Lead>[] = useMemo(() => [
    {
      accessorKey: "score",
      header: ({ column }) => (
        <Button variant="ghost" size="sm" onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}>
          Score <ArrowUpDown className="ml-1 size-3" />
        </Button>
      ),
      cell: ({ row }) => <ScoreBadge score={row.original.score} tier={row.original.score_tier} />,
      size: 56,
    },
    {
      accessorKey: "company",
      header: ({ column }) => (
        <Button variant="ghost" size="sm" onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}>
          Company <ArrowUpDown className="ml-1 size-3" />
        </Button>
      ),
      cell: ({ row }) => {
        const website = row.original.website
        let domain: string | undefined
        if (website) {
          try { domain = new URL(website.startsWith('http') ? website : `https://${website}`).hostname.replace('www.', '') } catch {}
        }
        return (
          <div className="max-w-[200px] flex items-center gap-1.5 truncate">
            {domain && (
              <img
                src={`https://www.google.com/s2/favicons?domain=${domain}&sz=16`}
                alt="" className="size-4 rounded shrink-0" loading="lazy"
                onError={(e) => { (e.target as HTMLImageElement).style.display = 'none' }}
              />
            )}
            <div className="min-w-0 truncate">
              <EditableCell
                value={row.original.company}
                onSave={(v) => updateLeadMut.mutate({ id: row.original.id, fields: { company: v } })}
                className="font-medium text-xs"
              />
              {row.original.specialization && (
                <span className="text-[10px] text-muted-foreground/60 ml-1 truncate">{row.original.specialization}</span>
              )}
            </div>
          </div>
        )
      },
      size: 200,
    },
    {
      accessorKey: "city",
      header: "City",
      cell: ({ row }) => <span className="text-xs text-muted-foreground">{row.original.city || "—"}</span>,
      size: 90,
    },
    {
      accessorKey: "email",
      header: "Email",
      cell: ({ row }) => (
        <EditableCell
          value={row.original.email || ""}
          onSave={(v) => updateLeadMut.mutate({ id: row.original.id, fields: { email: v } })}
          placeholder="Add email"
        />
      ),
    },
    {
      accessorKey: "phone",
      header: "Phone",
      cell: ({ row }) => (
        <EditableCell
          value={row.original.phone || ""}
          onSave={(v) => updateLeadMut.mutate({ id: row.original.id, fields: { phone: v } })}
          placeholder="Add phone"
        />
      ),
      size: 140,
    },
    {
      accessorKey: "website",
      header: "Web",
      cell: ({ row }) => row.original.website ? (
        <a
          href={row.original.website}
          target="_blank"
          rel="noopener noreferrer"
          className="text-muted-foreground hover:text-foreground"
          onClick={(e) => e.stopPropagation()}
        >
          <Globe className="size-4" />
        </a>
      ) : null,
      size: 50,
    },
    {
      accessorKey: "status",
      header: "Status",
      cell: ({ row }) => {
        const s = row.original.status || "new"
        return (
          <span className="inline-flex items-center gap-1.5 text-[11px] text-muted-foreground">
            <span className={`size-1.5 rounded-full ${STATUS_DOT[s] || STATUS_DOT.new}`} />
            {s}
          </span>
        )
      },
      size: 72,
    },
    // ── Optional / rich columns (hidden by default; toggle via "Columns") ──
    {
      id: "industry",
      header: "Industry",
      cell: ({ row }) => <span className="text-xs text-muted-foreground truncate">{row.original.industry_tags || "—"}</span>,
      size: 130,
    },
    {
      id: "size",
      header: "Size",
      cell: ({ row }) => {
        const s = row.original.company_size || (row.original.employee_count_exact ? `${row.original.employee_count_exact}` : "")
        return <span className="text-xs text-muted-foreground">{s || "—"}</span>
      },
      size: 80,
    },
    {
      id: "founded",
      header: "Founded",
      cell: ({ row }) => <span className="text-xs text-muted-foreground tabular-nums">{row.original.founded_year || "—"}</span>,
      size: 70,
    },
    {
      id: "revenue",
      header: "Revenue",
      cell: ({ row }) => <span className="text-xs text-muted-foreground">{row.original.revenue_range || "—"}</span>,
      size: 100,
    },
    {
      id: "glassdoor",
      header: "Glassdoor",
      cell: ({ row }) => {
        const g = row.original.glassdoor_rating
        return g ? <span className="text-xs text-amber-400 tabular-nums">★ {g}</span> : <span className="text-xs text-muted-foreground">—</span>
      },
      size: 80,
    },
    {
      id: "contact",
      header: "Contact",
      cell: ({ row }) => <span className="text-xs text-muted-foreground truncate">{row.original.contact_person || "—"}</span>,
      size: 120,
    },
    {
      id: "linkedin",
      header: "LinkedIn",
      cell: ({ row }) => row.original.linkedin_url ? (
        <a href={row.original.linkedin_url} target="_blank" rel="noopener noreferrer"
           className="text-sky-500 hover:text-sky-400 text-xs" onClick={(e) => e.stopPropagation()}>in</a>
      ) : <span className="text-xs text-muted-foreground">—</span>,
      size: 60,
    },
    {
      id: "signals",
      header: "Signals",
      cell: ({ row }) => {
        const raw = row.original.hiring_signals
        let count = 0
        if (raw) {
          try { const v = JSON.parse(raw); count = Array.isArray(v) ? v.length : (typeof v === "object" && v ? Object.keys(v).length : (v ? 1 : 0)) }
          catch { count = String(raw).trim() ? 1 : 0 }
        }
        return count > 0
          ? <Badge variant="secondary" className="text-[10px] gap-0.5"><Flame className="size-2.5 text-red-400" />{count}</Badge>
          : <span className="text-xs text-muted-foreground">—</span>
      },
      size: 80,
    },
    {
      id: "actions",
      cell: ({ row }) => (
        <DropdownMenu>
          <DropdownMenuTrigger>
            <span
              className="inline-flex items-center justify-center rounded-md p-1 hover:bg-muted transition-colors cursor-pointer"
              onClick={(e) => e.stopPropagation()}
            >
              <MoreHorizontal className="size-4" />
            </span>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={() => {
              updateStatusMut.mutate({ id: row.original.id, status: "contacted" })
              toast.success("Marked as contacted")
            }}>
              Mark contacted
            </DropdownMenuItem>
            <DropdownMenuItem onClick={() => {
              updateStatusMut.mutate({ id: row.original.id, status: "qualified" })
              toast.success("Marked as qualified")
            }}>
              Mark qualified
            </DropdownMenuItem>
            <DropdownMenuItem onClick={() => openSimilar(row.original)}>
              Find similar
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              className="text-destructive"
              onClick={() => {
                deleteLeadMut.mutate(row.original.id)
                toast.success("Lead deleted")
              }}
            >
              Delete
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      ),
      size: 50,
    },
  ], [updateStatusMut, deleteLeadMut, updateLeadMut, openSimilar])

  const handleCollect = () => {
    if (!collectQuery.trim()) return
    collect.mutate({ query: collectQuery })
    toast.success(`Collection started: "${collectQuery}"`)
    setCollectQuery("")
  }

  const saveCurrentSegment = () => {
    const name = window.prompt("Name this segment:")?.trim()
    if (!name) return
    const next = [...segments.filter(s => s.name !== name), { name, filters: f }]
    setSegments(next); saveSegments(next)
    toast.success(`Saved segment "${name}"`)
  }
  const applySegment = (name: string) => {
    const seg = segments.find(s => s.name === name)
    if (seg) setF({ ...EMPTY_FILTERS, ...seg.filters })
  }
  const deleteSegment = (name: string) => {
    const next = segments.filter(s => s.name !== name)
    setSegments(next); saveSegments(next)
  }

  const runDedupNow = async () => {
    setDedupRunning(true); setDedupData(null)
    const params: Record<string, string> = { limit: "2000" }
    if (f.city) params.city = f.city
    if (f.source) params.source = f.source
    if (f.tier) params.tier = f.tier
    try { setDedupData(await runDedup(params)) }
    catch { toast.error("Dedup failed") }
    finally { setDedupRunning(false) }
  }
  const openDedup = () => { setDedupOpen(true); runDedupNow() }
  const doMerge = async (s: DedupSuggestion) => {
    setMergingId(s.master_id)
    try {
      await mergeDuplicates(s.master_id, s.duplicate_ids)
      setDedupData(prev => prev && {
        ...prev,
        merge_suggestions: prev.merge_suggestions.filter(x => x.master_id !== s.master_id),
      })
      toast.success(`Merged ${s.duplicate_ids.length} into ${s.master_company}`)
      refetch()
    } catch { toast.error("Merge failed") }
    finally { setMergingId(null) }
  }

  return (
    <div className="flex flex-col gap-1.5 p-2 h-full overflow-hidden">
      {/* Row 1 — Stats + Collect + Filters (compact single row) */}
      <div className="flex items-center gap-2">
        {/* Stats */}
        {stats ? (
          <div className="flex items-center gap-2 text-sm shrink-0">
            <span className="font-semibold">{stats.total}</span>
            <span className="text-muted-foreground text-xs">leads</span>
            <Separator orientation="vertical" className="h-3.5" />
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <span className="flex items-center gap-0.5"><Flame className="size-3 text-red-500" />{stats.by_tier?.hot ?? 0}</span>
              <span className="flex items-center gap-0.5"><Sun className="size-3 text-orange-400" />{stats.by_tier?.warm ?? 0}</span>
              <span className="flex items-center gap-0.5"><Snowflake className="size-3 text-blue-400" />{stats.by_tier?.cold ?? 0}</span>
            </div>
            <Separator orientation="vertical" className="h-3.5" />
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <span className="flex items-center gap-0.5"><Mail className="size-3 text-emerald-500" />{stats.enrichment?.with_email ?? 0}</span>
              <span className="flex items-center gap-0.5"><Phone className="size-3 text-sky-500" />{stats.enrichment?.with_phone ?? 0}</span>
            </div>
          </div>
        ) : (
          <Skeleton className="h-4 w-48" />
        )}

        {/* Collect */}
        <div className="flex items-center gap-1 flex-1 max-w-sm ml-2">
          <Input
            placeholder="Collect leads… (e.g. IT staffing Pune)"
            value={collectQuery}
            onChange={(e) => setCollectQuery(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && handleCollect()}
            className="h-7 text-xs"
          />
          <Button size="sm" onClick={handleCollect} disabled={collect.isPending || !collectQuery.trim()} className="h-7 text-xs px-2">
            <Plus className="size-3" />
            Collect
          </Button>
        </div>

        {/* Filters + Actions */}
        <div className="flex items-center gap-1 ml-auto shrink-0">
          {filterOptions?.cities && (
            <Select value={f.city || "all"} onValueChange={(v) => setFilter("city", v === "all" ? "" : v ?? "")}>
              <SelectTrigger className="w-[110px] h-7 text-xs">
                <SelectValue placeholder="All cities" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All cities</SelectItem>
                {filterOptions.cities.map(c => (
                  <SelectItem key={c} value={c}>{c}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {filterOptions?.tiers && (
            <Select value={f.tier || "all"} onValueChange={(v) => setFilter("tier", v === "all" ? "" : v ?? "")}>
              <SelectTrigger className="w-[100px] h-7 text-xs">
                <SelectValue placeholder="All tiers" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All tiers</SelectItem>
                {filterOptions.tiers.map(t => (
                  <SelectItem key={t} value={t}>{t}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}

          {/* Advanced filters */}
          <Popover>
            <PopoverTrigger
              render={<Button variant="outline" size="sm" className="h-7 text-xs px-2 gap-1" />}
            >
              <SlidersHorizontal className="size-3" />
              Filters
              {activeFilterCount > 0 && (
                <span className="ml-0.5 rounded-full bg-primary/15 text-primary px-1.5 text-[10px] font-medium">{activeFilterCount}</span>
              )}
            </PopoverTrigger>
            <PopoverContent align="end" className="w-64 space-y-2.5 text-xs">
              <div className="flex items-center justify-between">
                <span className="font-medium">Filters</span>
                {activeFilterCount > 0 && (
                  <button className="text-muted-foreground hover:text-foreground inline-flex items-center gap-1"
                          onClick={() => setF(EMPTY_FILTERS)}>
                    <X className="size-3" /> Clear
                  </button>
                )}
              </div>

              <label className="block">Status
                <Select value={f.status || "all"} onValueChange={(v) => setFilter("status", v === "all" ? "" : (v ?? ""))}>
                  <SelectTrigger className="h-7 mt-0.5"><SelectValue placeholder="Any" /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">Any</SelectItem>
                    {(filterOptions?.statuses ?? ["new","contacted","qualified","negotiating","converted","dead"]).map(s => (
                      <SelectItem key={s} value={s}>{s}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </label>

              {filterOptions?.sources && (
                <label className="block">Source
                  <Select value={f.source || "all"} onValueChange={(v) => setFilter("source", v === "all" ? "" : (v ?? ""))}>
                    <SelectTrigger className="h-7 mt-0.5"><SelectValue placeholder="Any" /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="all">Any</SelectItem>
                      {filterOptions.sources.map(s => <SelectItem key={s} value={s}>{s}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </label>
              )}

              <div className="flex gap-2">
                <label className="flex-1">Score ≥
                  <Input type="number" min={0} max={100} value={f.scoreMin}
                         onChange={e => setFilter("scoreMin", e.target.value)} className="h-7 mt-0.5" />
                </label>
                <label className="flex-1">Score ≤
                  <Input type="number" min={0} max={100} value={f.scoreMax}
                         onChange={e => setFilter("scoreMax", e.target.value)} className="h-7 mt-0.5" />
                </label>
              </div>

              <label className="block">Email
                <Select value={f.hasEmail || "any"} onValueChange={(v) => setFilter("hasEmail", v === "any" ? "" : (v ?? ""))}>
                  <SelectTrigger className="h-7 mt-0.5"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="any">Any</SelectItem>
                    <SelectItem value="true">Has email</SelectItem>
                    <SelectItem value="false">Missing email</SelectItem>
                  </SelectContent>
                </Select>
              </label>

              <label className="block">Phone
                <Select value={f.hasPhone || "any"} onValueChange={(v) => setFilter("hasPhone", v === "any" ? "" : (v ?? ""))}>
                  <SelectTrigger className="h-7 mt-0.5"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="any">Any</SelectItem>
                    <SelectItem value="true">Has phone</SelectItem>
                    <SelectItem value="false">Missing phone</SelectItem>
                  </SelectContent>
                </Select>
              </label>
            </PopoverContent>
          </Popover>

          {/* Saved segments */}
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" size="sm" className="h-7 text-xs px-2 gap-1" />}>
              <Bookmark className="size-3" /> Segments
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={saveCurrentSegment} disabled={activeFilterCount === 0}>
                Save current as segment…
              </DropdownMenuItem>
              {segments.length > 0 && <DropdownMenuSeparator />}
              {segments.map(s => (
                <DropdownMenuItem key={s.name} onClick={() => applySegment(s.name)} className="justify-between gap-4">
                  <span className="truncate">{s.name}</span>
                  <X className="size-3 text-muted-foreground hover:text-destructive"
                     onClick={(e) => { e.stopPropagation(); deleteSegment(s.name) }} />
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>

          <Button variant="outline" size="sm" onClick={openDedup} className="h-7 text-xs px-2 gap-1">
            <GitMerge className="size-3" /> Dedup
          </Button>
          <Button variant="outline" size="sm" onClick={() => refetch()} className="h-7 w-7 p-0">
            <RefreshCw className="size-3" />
          </Button>
          <a href={exportCSVUrl(filters)} download>
            <Button variant="outline" size="sm" className="h-7 w-7 p-0">
              <Download className="size-3" />
            </Button>
          </a>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 text-xs px-2 gap-1"
                  disabled={importBR.isPending}
                />
              }
            >
              <Upload className="size-3" />
              🇧🇷
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={() => {
                importBR.mutate({ module: "all" })
                toast.success("🇧🇷 Importing all BR data (CNPJ + GitHub)...")
              }}>
                Import All (CNPJ + GitHub)
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => {
                importBR.mutate({ module: "cnpj", limit: 10000 })
                toast.success("🇧🇷 Importing first 10K CNPJ leads...")
              }}>
                CNPJ only (10K sample)
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => {
                importBR.mutate({ module: "github" })
                toast.success("🇧🇷 Importing GitHub leads...")
              }}>
                GitHub only
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {/* Data Table */}
      <div className="flex-1 min-h-0">
        {isLoading ? (
          <div className="space-y-2">
            {Array.from({ length: 10 }).map((_, i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        ) : (
          <DataTable
            columns={columns}
            data={leads ?? []}
            searchKey="company"
            searchPlaceholder="Filter companies..."
            onRowClick={(lead) => navigate(`/leads/${lead.id}`)}
            enableSelection
            onSelectionChange={setSelectedRows}
            columnVisibilityKey="yupcha:leadCols"
            initialColumnVisibility={{
              industry: false, size: false, founded: false, revenue: false,
              glassdoor: false, contact: false, linkedin: false, signals: false,
            }}
          />
        )}
      </div>

      {/* Bulk Actions Bar */}
      {selectedRows.length > 0 && (
        <div className="fixed bottom-4 left-1/2 -translate-x-1/2 z-50 flex items-center gap-2 rounded-lg border bg-background px-4 py-2 shadow-lg">
          <span className="text-sm font-medium">{selectedRows.length} selected</span>
          <Separator orientation="vertical" className="h-4" />
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              selectedRows.forEach(r => updateStatusMut.mutate({ id: r.id, status: "contacted" }))
              toast.success(`${selectedRows.length} leads marked as contacted`)
            }}
          >
            Mark Contacted
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              selectedRows.forEach(r => updateStatusMut.mutate({ id: r.id, status: "qualified" }))
              toast.success(`${selectedRows.length} leads marked as qualified`)
            }}
          >
            Mark Qualified
          </Button>
          <Button
            variant="destructive"
            size="sm"
            onClick={() => {
              selectedRows.forEach(r => deleteLeadMut.mutate(r.id))
              toast.success(`${selectedRows.length} leads deleted`)
              setSelectedRows([])
            }}
          >
            Delete
          </Button>
        </div>
      )}

      {/* Dedup dialog */}
      <Dialog open={dedupOpen} onOpenChange={setDedupOpen}>
        <DialogContent className="max-w-xl">
          <DialogHeader>
            <DialogTitle className="text-sm">Find &amp; merge duplicates</DialogTitle>
          </DialogHeader>
          <p className="text-[11px] text-muted-foreground -mt-1">
            Scans a bounded subset (current city/source/tier filters, up to 2,000 rows). Filter first to target a region.
          </p>
          {dedupRunning ? (
            <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground justify-center">
              <Loader2 className="size-4 animate-spin" /> Scanning for duplicates…
            </div>
          ) : !dedupData ? null : dedupData.merge_suggestions.length === 0 ? (
            <p className="text-sm text-muted-foreground py-4">
              No duplicates found in {dedupData.scanned ?? dedupData.stats.total_leads} scanned leads. 🎉
            </p>
          ) : (
            <>
              <div className="text-xs text-muted-foreground">
                {dedupData.stats.duplicates_found} duplicates in {dedupData.stats.clusters} clusters
                {" · "}scanned {dedupData.scanned ?? dedupData.stats.total_leads}
              </div>
              <div className="max-h-[55vh] overflow-auto space-y-1.5">
                {dedupData.merge_suggestions.map(s => (
                  <div key={s.master_id} className="flex items-center gap-2 rounded-md border p-2">
                    <div className="min-w-0 flex-1">
                      <div className="text-xs font-medium truncate">
                        {s.master_company}
                        <span className="ml-1.5 text-[10px] text-emerald-400">{Math.round(s.confidence * 100)}% match</span>
                      </div>
                      <div className="text-[10px] text-muted-foreground truncate">
                        merges {s.duplicate_ids.length}: {s.duplicate_companies.join(", ")}
                      </div>
                    </div>
                    <Button size="sm" variant="outline" className="h-7 text-xs px-2 shrink-0"
                            disabled={mergingId === s.master_id}
                            onClick={() => doMerge(s)}>
                      {mergingId === s.master_id ? <Loader2 className="size-3 animate-spin" /> : <GitMerge className="size-3" />}
                      Merge
                    </Button>
                  </div>
                ))}
              </div>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* Similar leads dialog */}
      <Dialog open={similarOpen} onOpenChange={setSimilarOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle className="text-sm">
              {similar ? `Leads similar to ${similar.reference}` : "Finding similar leads…"}
            </DialogTitle>
          </DialogHeader>
          {!similar ? (
            <div className="space-y-2 py-2">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-8 w-full" />)}
            </div>
          ) : similar.similar_leads.length === 0 ? (
            <p className="text-sm text-muted-foreground py-4">No similar leads found.</p>
          ) : (
            <div className="max-h-[60vh] overflow-auto divide-y">
              {similar.similar_leads.map(l => (
                <button
                  key={l.id}
                  onClick={() => { setSimilarOpen(false); navigate(`/leads/${l.id}`) }}
                  className="flex items-center gap-3 w-full text-left py-2 px-1 hover:bg-muted/50 rounded transition-colors"
                >
                  <ScoreBadge score={l.score} tier={l.score_tier} />
                  <div className="min-w-0 flex-1">
                    <div className="text-xs font-medium truncate">{l.company}</div>
                    <div className="text-[10px] text-muted-foreground truncate">
                      {[l.city, l.specialization].filter(Boolean).join(" · ") || "—"}
                    </div>
                  </div>
                  {l.email && <Mail className="size-3 text-emerald-500 shrink-0" />}
                </button>
              ))}
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}
