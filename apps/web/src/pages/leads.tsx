import { useState, useMemo } from "react"
import { useNavigate } from "react-router-dom"
import { type ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import {
  ArrowUpDown, ExternalLink, Mail, Phone, MoreHorizontal,
  Plus, Download, RefreshCw, Globe, Flame, Sun, Snowflake, Upload,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { DataTable } from "@/components/data-table"
import { useLeads, useStats, useFilters, useUpdateStatus, useUpdateLead, useDeleteLead, useCollect, useImportDataCollector } from "@/lib/hooks"
import { exportCSVUrl, type Lead } from "@/lib/api"
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

export default function LeadsPage() {
  const navigate = useNavigate()
  const [city, setCity] = useState("")
  const [tier, setTier] = useState("")
  const [collectQuery, setCollectQuery] = useState("")
  const [selectedRows, setSelectedRows] = useState<Lead[]>([])

  const filters: Record<string, string> = {
    limit: "500",
    order_by: "score DESC",
    ...(city && { city }),
    ...(tier && { tier }),
  }

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
  ], [updateStatusMut, deleteLeadMut])

  const handleCollect = () => {
    if (!collectQuery.trim()) return
    collect.mutate({ query: collectQuery })
    toast.success(`Collection started: "${collectQuery}"`)
    setCollectQuery("")
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
            <Select value={city || "all"} onValueChange={(v) => setCity(v === "all" ? "" : v ?? "")}>
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
            <Select value={tier || "all"} onValueChange={(v) => setTier(v === "all" ? "" : v ?? "")}>
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
    </div>
  )
}
