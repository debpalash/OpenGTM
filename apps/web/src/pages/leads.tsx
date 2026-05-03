import { useState, useMemo } from "react"
import { useNavigate } from "react-router-dom"
import { type ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import {
  ArrowUpDown, ExternalLink, Mail, Phone, MoreHorizontal,
  Plus, Download, RefreshCw, Globe,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
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
import { useLeads, useStats, useFilters, useUpdateStatus, useUpdateLead, useDeleteLead, useCollect } from "@/lib/hooks"
import { exportCSVUrl, type Lead } from "@/lib/api"
import { EditableCell } from "@/components/editable-cell"

const TIER_COLORS: Record<string, string> = {
  hot:          "bg-red-500/10 text-red-500 border-red-500/20",
  warm:         "bg-orange-500/10 text-orange-500 border-orange-500/20",
  cold:         "bg-blue-500/10 text-blue-500 border-blue-500/20",
  unqualified:  "bg-muted text-muted-foreground",
}

function ScoreBadge({ score, tier }: { score: number; tier: string }) {
  return (
    <Badge variant="outline" className={TIER_COLORS[tier] || TIER_COLORS.cold}>
      {score}
    </Badge>
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

  const columns: ColumnDef<Lead>[] = useMemo(() => [
    {
      accessorKey: "score",
      header: ({ column }) => (
        <Button variant="ghost" size="sm" onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}>
          Score <ArrowUpDown className="ml-1 size-3" />
        </Button>
      ),
      cell: ({ row }) => <ScoreBadge score={row.original.score} tier={row.original.score_tier} />,
      size: 80,
    },
    {
      accessorKey: "company",
      header: ({ column }) => (
        <Button variant="ghost" size="sm" onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}>
          Company <ArrowUpDown className="ml-1 size-3" />
        </Button>
      ),
      cell: ({ row }) => (
        <div className="max-w-[200px]">
          <EditableCell
            value={row.original.company}
            onSave={(v) => updateLeadMut.mutate({ id: row.original.id, fields: { company: v } })}
            className="font-medium"
          />
          {row.original.specialization && (
            <div className="text-xs text-muted-foreground truncate">{row.original.specialization}</div>
          )}
        </div>
      ),
    },
    {
      accessorKey: "city",
      header: "City",
      cell: ({ row }) => <span className="text-sm">{row.original.city || "—"}</span>,
      size: 100,
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
      cell: ({ row }) => (
        <Badge variant="outline" className="text-xs">
          {row.original.status || "new"}
        </Badge>
      ),
      size: 80,
    },
    {
      id: "actions",
      cell: ({ row }) => (
        <DropdownMenu>
          <DropdownMenuTrigger>
            <button
              className="inline-flex items-center justify-center rounded-md p-1 hover:bg-muted transition-colors"
              onClick={(e) => e.stopPropagation()}
            >
              <MoreHorizontal className="size-4" />
            </button>
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
    <div className="flex flex-col gap-4 p-4">
      {/* Stats Bar */}
      <div className="flex items-center gap-4 text-sm">
        {stats ? (
          <>
            <div><span className="font-medium">{stats.total}</span> <span className="text-muted-foreground">leads</span></div>
            <Separator orientation="vertical" className="h-4" />
            <div className="text-muted-foreground">
              🔥 {stats.by_tier?.hot ?? 0} ·{" "}
              🟡 {stats.by_tier?.warm ?? 0} ·{" "}
              🔵 {stats.by_tier?.cold ?? 0}
            </div>
            <Separator orientation="vertical" className="h-4" />
            <div className="text-muted-foreground">
              📧 {stats.enrichment?.with_email ?? 0} · 📞 {stats.enrichment?.with_phone ?? 0}
            </div>
          </>
        ) : (
          <Skeleton className="h-4 w-64" />
        )}
      </div>

      {/* Collect + Filters Bar */}
      <div className="flex items-center gap-2">
        <div className="flex items-center gap-1 flex-1 max-w-md">
          <Input
            placeholder="Collect leads... (e.g. IT staffing companies Pune)"
            value={collectQuery}
            onChange={(e) => setCollectQuery(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && handleCollect()}
          />
          <Button size="sm" onClick={handleCollect} disabled={collect.isPending || !collectQuery.trim()}>
            <Plus className="size-4" />
            Collect
          </Button>
        </div>
        <div className="flex items-center gap-1 ml-auto">
          {filterOptions?.cities && (
            <Select value={city || "all"} onValueChange={(v) => setCity(v === "all" ? "" : v ?? "")}>
              <SelectTrigger className="w-[130px] h-8 text-xs">
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
              <SelectTrigger className="w-[110px] h-8 text-xs">
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
          <Button variant="outline" size="sm" onClick={() => refetch()}>
            <RefreshCw className="size-3" />
          </Button>
          <a href={exportCSVUrl(filters)} download>
            <Button variant="outline" size="sm">
              <Download className="size-3" />
            </Button>
          </a>
        </div>
      </div>

      {/* Data Table */}
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
