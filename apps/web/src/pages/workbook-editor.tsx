/**
 * Workbook Editor — Hybrid model.
 *
 * Rows are leads from the DB. Columns show lead fields + enrichment overlays.
 * Edits to lead_field columns write back to the Lead record.
 * AI/enrichment columns show overlay data.
 */

import { useState, useMemo, useCallback, useRef, useEffect } from "react"
import { useParams, useNavigate } from "react-router-dom"
import {
  useReactTable, getCoreRowModel, getSortedRowModel, getFilteredRowModel,
  flexRender, type ColumnDef, type CellContext, type SortingState,
} from "@tanstack/react-table"
import { useVirtualizer } from "@tanstack/react-virtual"
import {
  useWorkbook, useUpdateWorkbook, useUpdateLeadField,
  useImportLeads, useRunWorkbook, useStopWorkbook,
  useDeleteLeads, useWorkbookSocket,
} from "@/lib/workbook-hooks"
import type { WorkbookLeadRow, ColumnConfig, EnrichmentOverlay } from "@/lib/workbook-api"
import {
  ArrowLeft, Plus, Play, Square, Download, Upload,
  Sparkles, Type, Layers, Brain, GitBranch, Send,
  Loader2, Check, X, AlertCircle, Clock, MoreHorizontal,
  FileSpreadsheet, ExternalLink, Filter, Search, Trash2, Copy,
  ArrowUpDown, ArrowUp, ArrowDown, EyeOff, Pencil,
} from "lucide-react"
import { toast } from "sonner"
import Papa from "papaparse"

// ── Column Type Metadata ─────────────────────────────────────────────────

const COL_TYPE_META: Record<string, { icon: typeof Type; color: string; label: string; headerBg: string }> = {
  lead_field: { icon: Type, color: "text-zinc-400", label: "Lead Field", headerBg: "" },
  input: { icon: Type, color: "text-zinc-400", label: "Lead Field", headerBg: "" },  // Legacy alias
  enrichment: { icon: Sparkles, color: "text-violet-400", label: "Enrichment", headerBg: "bg-violet-500/5" },
  waterfall: { icon: Layers, color: "text-blue-400", label: "Waterfall", headerBg: "bg-blue-500/5" },
  ai_formula: { icon: Brain, color: "text-amber-400", label: "AI Formula", headerBg: "bg-amber-500/5" },
  conditional: { icon: GitBranch, color: "text-emerald-400", label: "Conditional", headerBg: "bg-emerald-500/5" },
  output: { icon: Send, color: "text-rose-400", label: "Output", headerBg: "bg-rose-500/5" },
}

// Fields that get type-aware rendering (module-level constant)
const TYPED_FIELDS = new Set(["website", "linkedin_url", "twitter_url", "facebook_url", "email", "score", "score_tier", "status"])

// ── Cell Status Indicator ────────────────────────────────────────────────

function CellStatus({ status }: { status?: string }) {
  if (!status || status === "complete") return null
  if (status === "running") return <Loader2 className="size-3 animate-spin text-blue-400 shrink-0" />
  if (status === "error") return <AlertCircle className="size-3 text-destructive shrink-0" />
  if (status === "pending") return <Clock className="size-3 text-muted-foreground/40 shrink-0" />
  if (status === "skipped") return <span className="size-3 text-muted-foreground/40 shrink-0 text-[10px] leading-3">—</span>
  return null
}

// ── Editable Cell ────────────────────────────────────────────────────────

function EditableCell({
  value, status, provider, error, isEditable, onSave,
}: {
  value: any; status?: string; provider?: string | null; error?: string | null
  isEditable: boolean; onSave: (v: string) => void
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(String(value ?? ""))
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (editing) inputRef.current?.focus()
  }, [editing])

  useEffect(() => {
    setDraft(String(value ?? ""))
  }, [value])

  const displayValue = useMemo(() => {
    if (value == null || value === "") return ""
    const str = typeof value === "string" ? value : JSON.stringify(value)
    if (str.startsWith("[{") || str.startsWith("{")) {
      try {
        const parsed = JSON.parse(str)
        if (Array.isArray(parsed)) {
          const first = parsed[0]
          if (first?.name) return `${first.name}${parsed.length > 1 ? ` +${parsed.length - 1} more` : ""}`
          if (first?.email) return first.email
          return `${parsed.length} items`
        }
        if (parsed.name) return parsed.name
        if (parsed.email) return parsed.email
        return str.slice(0, 80)
      } catch { /* fall through */ }
    }
    return str
  }, [value])

  if (editing && isEditable) {
    return (
      <input
        ref={inputRef}
        value={draft}
        onChange={e => setDraft(e.target.value)}
        onBlur={() => { onSave(draft); setEditing(false) }}
        onKeyDown={e => {
          if (e.key === "Enter") { onSave(draft); setEditing(false) }
          if (e.key === "Escape") setEditing(false)
        }}
        className="w-full h-full px-2 py-1 text-sm bg-transparent border-0 focus:outline-none focus:ring-1 focus:ring-primary/50 rounded"
      />
    )
  }

  return (
    <div
      className="flex items-center gap-1.5 px-2 py-1 h-full min-h-[32px] max-w-full cursor-default group/cell overflow-hidden"
      onDoubleClick={() => isEditable && setEditing(true)}
      title={error ? `Error: ${error}` : displayValue || (provider ? `via ${provider}` : undefined)}
    >
      <CellStatus status={status} />
      <span className="truncate text-sm flex-1 min-w-0">
        {displayValue}
      </span>
      {displayValue && (
        <button
          onClick={(e) => {
            e.stopPropagation()
            navigator.clipboard.writeText(displayValue)
            toast.success("Copied", { duration: 1200 })
          }}
          className="hidden group-hover/cell:inline-flex p-0.5 rounded hover:bg-muted shrink-0"
          title="Copy"
        >
          <Copy className="size-3 text-muted-foreground" />
        </button>
      )}
      {provider && status === "complete" && (
        <span className="hidden group-hover/cell:inline text-[10px] text-muted-foreground/50 shrink-0">
          {provider}
        </span>
      )}
    </div>
  )
}
// ── Type-Aware Cell Formatters ──────────────────────────────────────────

function TypedCellValue({ value, fieldName }: { value: string; fieldName: string }) {
  if (!value || value === "") return <span className="text-muted-foreground/30">—</span>

  // URL fields → clickable link showing domain only
  if (fieldName === "website" || fieldName === "linkedin_url" || fieldName === "twitter_url" || fieldName === "facebook_url") {
    try {
      const url = value.startsWith("http") ? value : `https://${value}`
      const domain = new URL(url).hostname.replace("www.", "")
      const display = fieldName.includes("linkedin") ? domain.replace("linkedin.com/in/", "") 
        : fieldName.includes("twitter") ? domain.replace("twitter.com/", "@")
        : domain
      return (
        <a href={url} target="_blank" rel="noopener" onClick={e => e.stopPropagation()}
          className="text-blue-400 hover:text-blue-300 hover:underline truncate text-sm">
          {display}
        </a>
      )
    } catch { /* fall through */ }
  }

  // Email → mailto link
  if (fieldName === "email" && value.includes("@")) {
    return (
      <a href={`mailto:${value}`} onClick={e => e.stopPropagation()}
        className="text-blue-400 hover:text-blue-300 hover:underline truncate text-sm font-mono text-[13px]">
        {value}
      </a>
    )
  }

  // Score → colored badge
  if (fieldName === "score" || fieldName === "score_tier") {
    const num = Number(value)
    if (!isNaN(num)) {
      const color = num >= 80 ? "bg-emerald-500/15 text-emerald-400" : num >= 50 ? "bg-amber-500/15 text-amber-400" : "bg-zinc-500/15 text-zinc-400"
      return <span className={`inline-flex px-1.5 py-0.5 rounded text-[11px] font-medium tabular-nums ${color}`}>{num}</span>
    }
    // score_tier text
    const tierColor = value === "hot" ? "text-emerald-400" : value === "warm" ? "text-amber-400" : "text-zinc-400"
    return <span className={`text-sm ${tierColor}`}>{value}</span>
  }

  // Status → colored dot
  if (fieldName === "status") {
    const statusColor = value === "new" ? "bg-blue-400" : value === "contacted" ? "bg-amber-400" : value === "qualified" ? "bg-emerald-400" : value === "lost" ? "bg-red-400" : "bg-zinc-400"
    return (
      <span className="inline-flex items-center gap-1.5 text-sm">
        <span className={`size-2 rounded-full ${statusColor}`} />
        {value}
      </span>
    )
  }

  return null // fallback to default EditableCell display
}

// ── Main Editor Page ─────────────────────────────────────────────────────

export default function WorkbookEditorPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const PAGE_SIZE = 1000
  const { data, isLoading, error } = useWorkbook(id!)
  const updateWb = useUpdateWorkbook()
  const updateLeadField = useUpdateLeadField(id!)
  const importLeadsMut = useImportLeads(id!)
  const runMut = useRunWorkbook(id!)
  const stopMut = useStopWorkbook(id!)
  const deleteMut = useDeleteLeads(id!)
  const { connected } = useWorkbookSocket(id)
  const tableContainerRef = useRef<HTMLDivElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [showColPicker, setShowColPicker] = useState(false)
  const [newColName, setNewColName] = useState("")
  const [newColType, setNewColType] = useState<"lead_field" | "ai_formula" | "waterfall" | "enrichment">("lead_field")
  const [newColLeadField, setNewColLeadField] = useState("")
  const [newColPrompt, setNewColPrompt] = useState("")
  const [newColCondition, setNewColCondition] = useState("")
  const [rowSelection, setRowSelection] = useState<Record<string, boolean>>({})
  const [sorting, setSorting] = useState<SortingState>([])
  const [globalFilter, setGlobalFilter] = useState("")
  const [hiddenColumns, setHiddenColumns] = useState<Set<string>>(new Set())
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number; colId: string } | null>(null)

  // Column resize state — uses a ref to avoid re-rendering entire table on drag
  const columnWidthsRef = useRef<Record<string, number>>({})
  const [, forceResizeRender] = useState(0) // only triggers on mouseUp
  const resizeRef = useRef<{ colId: string; startX: number; startW: number } | null>(null)

  const workbook = data?.workbook
  const rows = data?.rows ?? []
  const columns = workbook?.columns_config ?? []

  // Initialize column widths from config (once)
  useEffect(() => {
    if (columns.length > 0 && Object.keys(columnWidthsRef.current).length === 0) {
      for (const col of columns) {
        columnWidthsRef.current[col.id] = col.width || 180
      }
    }
  }, [columns])

  // Get width for a column (reads ref, no re-render)
  const getColWidth = useCallback((colId: string, defaultW: number = 180) => {
    return columnWidthsRef.current[colId] || defaultW
  }, [])

  // Column resize handlers — uses direct DOM mutation during drag for zero re-renders
  const handleResizeStart = useCallback((colId: string, e: React.MouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
    const startW = columnWidthsRef.current[colId] || 180
    resizeRef.current = { colId, startX: e.clientX, startW }

    const onMouseMove = (ev: MouseEvent) => {
      if (!resizeRef.current) return
      const diff = ev.clientX - resizeRef.current.startX
      const newW = Math.max(80, Math.min(600, resizeRef.current.startW + diff))
      columnWidthsRef.current[resizeRef.current.colId] = newW
      // Direct DOM update — zero React re-renders during drag
      const container = tableContainerRef.current
      if (container) {
        const cells = container.querySelectorAll(`[data-col-id="${resizeRef.current.colId}"]`)
        cells.forEach(cell => {
          ;(cell as HTMLElement).style.width = `${newW}px`
          ;(cell as HTMLElement).style.maxWidth = `${newW}px`
        })
      }
    }
    const onMouseUp = () => {
      resizeRef.current = null
      document.removeEventListener("mousemove", onMouseMove)
      document.removeEventListener("mouseup", onMouseUp)
      document.body.style.cursor = ""
      document.body.style.userSelect = ""
      forceResizeRender(n => n + 1) // single re-render on release
    }
    document.addEventListener("mousemove", onMouseMove)
    document.addEventListener("mouseup", onMouseUp)
    document.body.style.cursor = "col-resize"
    document.body.style.userSelect = "none"
  }, [])

  // ── Build TanStack Table columns ───────────────────────────────────────

  const tableColumns = useMemo<ColumnDef<WorkbookLeadRow>[]>(() => {
    const cols: ColumnDef<WorkbookLeadRow>[] = [
      // Checkbox column
      {
        id: "_select",
        header: ({ table }) => (
          <div className="flex items-center justify-center px-1">
            <input
              type="checkbox"
              className="size-3.5 rounded border-border accent-primary cursor-pointer"
              checked={table.getIsAllRowsSelected()}
              onChange={table.getToggleAllRowsSelectedHandler()}
            />
          </div>
        ),
        size: 36,
        cell: ({ row }) => (
          <div className="flex items-center justify-center px-1">
            <input
              type="checkbox"
              className="size-3.5 rounded border-border accent-primary cursor-pointer"
              checked={row.getIsSelected()}
              onChange={row.getToggleSelectedHandler()}
            />
          </div>
        ),
      },
      // Row number column
      {
        id: "_index",
        header: "#",
        size: 40,
        cell: ({ row }) => (
          <div className="flex items-center gap-1 px-1.5">
            <span className="text-[10px] text-muted-foreground tabular-nums">
              {row.index + 1}
            </span>
            <a
              href={`/leads/${row.original.lead_id}`}
              className="opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-foreground transition-all"
              title="Open lead detail"
              onClick={e => e.stopPropagation()}
            >
              <ExternalLink className="size-3" />
            </a>
          </div>
        ),
      },
      // Dynamic columns from workbook config
      ...columns.filter(col => !hiddenColumns.has(col.id)).map((col) => {
        const meta = COL_TYPE_META[col.type] || COL_TYPE_META.lead_field
        const Icon = meta.icon

        return {
          id: col.id,
          accessorFn: (row: WorkbookLeadRow) => {
            if (col.type === "lead_field" || col.type === "input") {
              return row.lead[col.lead_field || col.id] ?? ""
            }
            return row.enrichments?.[col.id]?.value ?? ""
          },
          header: ({ column }: any) => {
            const sort = column.getIsSorted()
            return (
              <div
                className={`flex items-center gap-1 px-2 h-full cursor-pointer select-none ${meta.headerBg}`}
                onClick={column.getToggleSortingHandler()}
                onContextMenu={(e) => {
                  e.preventDefault()
                  setCtxMenu({ x: e.clientX, y: e.clientY, colId: col.id })
                }}
              >
                <Icon className={`size-3 ${meta.color} shrink-0`} />
                <span className="truncate text-xs font-medium flex-1">{col.name}</span>
                {sort === "asc" && <ArrowUp className="size-3 text-primary shrink-0" />}
                {sort === "desc" && <ArrowDown className="size-3 text-primary shrink-0" />}
                {!sort && <ArrowUpDown className="size-3 text-muted-foreground/30 shrink-0 opacity-0 group-hover/th:opacity-100 transition-opacity" />}
              </div>
            )
          },
          cell: ({ row }: CellContext<WorkbookLeadRow, unknown>) => {
            // For lead_field columns (or legacy "input" type), get value from lead data
            if (col.type === "lead_field" || col.type === "input") {
              const leadField = col.lead_field || col.id
              const value = row.original.lead[leadField] ?? ""
              const strVal = String(value)

              // Type-aware fields get special rendering
              if (strVal && TYPED_FIELDS.has(leadField)) {
                return (
                  <div className="flex items-center gap-1.5 px-2 py-1 h-full min-h-[32px] max-w-full overflow-hidden"
                    title={strVal}>
                    <TypedCellValue value={strVal} fieldName={leadField} />
                  </div>
                )
              }

              // Fallback: editable text cell
              return (
                <EditableCell
                  value={value}
                  isEditable={true}
                  onSave={(v) => {
                    updateLeadField.mutate({
                      leadId: row.original.lead_id,
                      fields: { [leadField]: v },
                    })
                  }}
                />
              )
            }

            // For enrichment/AI columns, get from overlay
            const overlay: EnrichmentOverlay = row.original.enrichments?.[col.id] || {
              value: null,
              status: "pending",
            }

            // Fallback: if enrichment has no value, check if lead already has this field
            const leadFallback = overlay.value ? null : (row.original.lead[col.id] || row.original.lead[col.lead_field || ""] || null)
            const displayValue = overlay.value || (leadFallback ? String(leadFallback) : null)
            const displayStatus = overlay.value ? overlay.status : (leadFallback ? "complete" : overlay.status)

            return (
              <EditableCell
                value={displayValue}
                status={displayStatus}
                provider={overlay.value ? overlay.provider : (leadFallback ? "lead" : null)}
                error={leadFallback ? null : overlay.error}
                isEditable={false}
                onSave={() => {}}
              />
            )
          },
        } satisfies ColumnDef<WorkbookLeadRow>
      }),
    ]
    return cols
  }, [columns, updateLeadField])

  const table = useReactTable({
    data: rows,
    columns: tableColumns,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    state: { rowSelection, sorting, globalFilter },
    onRowSelectionChange: setRowSelection,
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
    globalFilterFn: (row, columnId, filterValue) => {
      const search = String(filterValue).toLowerCase()
      // Search across all lead fields + enrichments
      const lead = row.original.lead
      for (const v of Object.values(lead)) {
        if (v && String(v).toLowerCase().includes(search)) return true
      }
      for (const e of Object.values(row.original.enrichments || {})) {
        if ((e as any)?.value && String((e as any).value).toLowerCase().includes(search)) return true
      }
      return false
    },
    getRowId: (row) => String(row.lead_id),
  })

  const selectedCount = Object.keys(rowSelection).filter(k => rowSelection[k]).length

  const handleExportSelected = useCallback(() => {
    const selectedRows = table.getSelectedRowModel().rows.map(r => r.original)
    if (!selectedRows.length) return
    const headers = columns.map(c => c.name)
    const csvRows = selectedRows.map(row =>
      columns.map(col => {
        if (col.type === "lead_field") return row.lead[col.lead_field || col.id] ?? ""
        return row.enrichments?.[col.id]?.value ?? ""
      })
    )
    const csv = Papa.unparse({ fields: headers, data: csvRows })
    const blob = new Blob([csv], { type: "text/csv" })
    const url = URL.createObjectURL(blob)
    const a = document.createElement("a")
    a.href = url
    a.download = `${workbook?.name || "export"}_selected.csv`
    a.click()
    URL.revokeObjectURL(url)
    toast.success(`Exported ${selectedRows.length} rows`)
  }, [table, columns, workbook])

  const handleDeleteSelected = useCallback(() => {
    const selectedRows = table.getSelectedRowModel().rows.map(r => r.original)
    if (!selectedRows.length) return
    if (!confirm(`Delete ${selectedRows.length} leads permanently?`)) return
    deleteMut.mutate(
      selectedRows.map(r => r.lead_id),
      {
        onSuccess: (data) => {
          toast.success(`Deleted ${data.deleted} leads`)
          setRowSelection({})
        },
        onError: () => toast.error("Failed to delete leads"),
      }
    )
  }, [table, deleteMut])

  const handleDeleteColumn = useCallback((colId: string) => {
    const col = columns.find(c => c.id === colId)
    if (!col) return
    if (!confirm(`Delete column "${col.name}"?`)) return
    updateWb.mutate({
      id: workbook!.id,
      columns_config: columns.filter(c => c.id !== colId) as any,
    })
    setCtxMenu(null)
  }, [columns, updateWb, workbook])

  const handleHideColumn = useCallback((colId: string) => {
    setHiddenColumns(prev => new Set([...prev, colId]))
    setCtxMenu(null)
  }, [])

  // Close context menu on click outside
  useEffect(() => {
    if (!ctxMenu) return
    const close = () => setCtxMenu(null)
    window.addEventListener("click", close)
    return () => window.removeEventListener("click", close)
  }, [ctxMenu])

  // ── Virtual scrolling ──────────────────────────────────────────────────

  const { getVirtualItems, getTotalSize } = useVirtualizer({
    count: table.getRowModel().rows.length,
    getScrollElement: () => tableContainerRef.current,
    estimateSize: () => 36,
    overscan: 20,
  })



  // ── CSV Import ─────────────────────────────────────────────────────────

  const handleCSVImport = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return

    Papa.parse(file, {
      header: true,
      skipEmptyLines: true,
      complete: async (results) => {
        if (!results.data?.length) {
          toast.error("Empty CSV file")
          return
        }

        try {
          const result = await importLeadsMut.mutateAsync(results.data as Record<string, any>[])
          toast.success(`Imported ${result.created} leads`)
        } catch {
          toast.error("CSV import failed")
        }
      },
      error: () => toast.error("Failed to parse CSV"),
    })

    e.target.value = ""
  }, [importLeadsMut])

  // ── CSV Export ─────────────────────────────────────────────────────────

  const handleExport = useCallback(() => {
    if (!rows.length) return
    const headers = columns.map(c => c.name)
    const csvRows = rows.map(row =>
      columns.map(col => {
        if (col.type === "lead_field") {
          return row.lead[col.lead_field || col.id] ?? ""
        }
        return row.enrichments?.[col.id]?.value ?? ""
      })
    )

    const csv = Papa.unparse({ fields: headers, data: csvRows })
    const blob = new Blob([csv], { type: "text/csv" })
    const url = URL.createObjectURL(blob)
    const a = document.createElement("a")
    a.href = url
    a.download = `${workbook?.name || "export"}.csv`
    a.click()
    URL.revokeObjectURL(url)
    toast.success("Exported CSV")
  }, [rows, columns, workbook])

  // ── Loading / Error ────────────────────────────────────────────────────

  if (isLoading) {
    return (
      <div className="flex items-center justify-center h-full">
        <Loader2 className="size-6 animate-spin text-primary" />
      </div>
    )
  }

  if (error || !workbook) {
    return (
      <div className="flex flex-col items-center justify-center h-full gap-4">
        <AlertCircle className="size-8 text-destructive" />
        <p className="text-sm text-muted-foreground">Workbook not found</p>
        <button onClick={() => navigate("/workbooks")} className="text-sm text-primary hover:underline">
          ← Back to workbooks
        </button>
      </div>
    )
  }

  const isRunning = workbook.status === "running"
  const filterDesc = workbook.filter_criteria
    ? Object.entries(workbook.filter_criteria).filter(([, v]) => v).map(([k, v]) => `${k}: ${v}`).join(", ")
    : "All leads"

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* ── Selection Toolbar ─────────────────────────────────────────── */}
      {selectedCount > 0 && (
        <div className="flex items-center gap-3 px-4 py-1.5 border-b bg-primary/5 shrink-0">
          <span className="text-xs font-medium text-primary tabular-nums">{selectedCount} selected</span>
          <button
            onClick={handleExportSelected}
            className="inline-flex items-center gap-1 px-2 py-1 rounded text-[11px] hover:bg-primary/10 text-primary transition-colors"
          >
            <Download className="size-3" /> Export Selected
          </button>
          <button
            onClick={handleDeleteSelected}
            className="inline-flex items-center gap-1 px-2 py-1 rounded text-[11px] hover:bg-destructive/10 text-destructive transition-colors"
          >
            <Trash2 className="size-3" /> Delete
          </button>
          <button
            onClick={() => setRowSelection({})}
            className="inline-flex items-center gap-1 px-2 py-1 rounded text-[11px] hover:bg-muted text-muted-foreground transition-colors"
          >
            <X className="size-3" /> Deselect
          </button>
        </div>
      )}

      {/* ── Toolbar ─────────────────────────────────────────────────────── */}
      <div className="flex items-center gap-3 px-4 py-2 border-b bg-background/95 backdrop-blur-sm shrink-0">
        <button
          onClick={() => navigate("/workbooks")}
          className="p-1.5 rounded-md hover:bg-muted transition-colors"
        >
          <ArrowLeft className="size-4" />
        </button>

        <div className="flex-1 min-w-0">
          <h2 className="text-sm font-semibold truncate">{workbook.name}</h2>
          <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
            <span className="flex items-center gap-1">
              <Filter className="size-3" />
              {filterDesc}
            </span>
            <span>·</span>
            <span>{data?.total_rows ?? 0} leads</span>
            <span>·</span>
            <span>{columns.length} columns</span>
          </div>
        </div>

        {/* Search Bar */}
        <div className="relative">
          <Search className="absolute left-2 top-1/2 -translate-y-1/2 size-3.5 text-muted-foreground pointer-events-none" />
          <input
            type="text"
            placeholder="Search rows..."
            value={globalFilter}
            onChange={e => setGlobalFilter(e.target.value)}
            className="w-44 pl-7 pr-2 py-1.5 rounded-md border bg-background text-xs focus:outline-none focus:ring-1 focus:ring-primary/50 placeholder:text-muted-foreground/50"
          />
          {globalFilter && (
            <button
              onClick={() => setGlobalFilter("")}
              className="absolute right-1.5 top-1/2 -translate-y-1/2 p-0.5 rounded hover:bg-muted"
            >
              <X className="size-3 text-muted-foreground" />
            </button>
          )}
        </div>

        {/* Hidden columns pills */}
        {hiddenColumns.size > 0 && (
          <div className="flex items-center gap-1">
            <span className="text-[10px] text-muted-foreground">{hiddenColumns.size} hidden</span>
            <button
              onClick={() => setHiddenColumns(new Set())}
              className="text-[10px] text-primary hover:underline"
            >
              Show all
            </button>
          </div>
        )}

        {/* Actions */}
        <div className="flex items-center gap-1.5">
          <input
            ref={fileInputRef}
            type="file"
            accept=".csv"
            onChange={handleCSVImport}
            className="hidden"
          />
          <button
            onClick={() => fileInputRef.current?.click()}
            className="inline-flex items-center gap-1.5 px-2.5 py-1.5 rounded-md text-xs hover:bg-muted transition-colors"
            title="Import CSV (creates new leads)"
          >
            <Upload className="size-3.5" />
            Import
          </button>

          <button
            onClick={handleExport}
            className="inline-flex items-center gap-1.5 px-2.5 py-1.5 rounded-md text-xs hover:bg-muted transition-colors"
            title="Export CSV"
          >
            <Download className="size-3.5" />
            Export
          </button>

          <div className="w-px h-5 bg-border mx-1" />

          {isRunning ? (
            <button
              onClick={() => stopMut.mutate()}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs bg-destructive/10 text-destructive hover:bg-destructive/20 transition-colors"
            >
              <Square className="size-3.5" />
              Stop
            </button>
          ) : (
            <button
              onClick={() => {
                runMut.mutate(undefined, {
                  onSuccess: (data) => toast.success(data.message),
                  onError: () => toast.error("Failed to start enrichment"),
                })
              }}
              disabled={runMut.isPending}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50 transition-colors"
            >
              {runMut.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Play className="size-3.5" />}
              Run Enrichment
            </button>
          )}
        </div>
      </div>

      {/* ── Table ───────────────────────────────────────────────────────── */}
      <div ref={tableContainerRef} className="flex-1 overflow-auto pl-2">
        <table className="border-collapse text-sm" style={{ tableLayout: "fixed", minWidth: "100%" }}>
          <thead className="sticky top-0 z-10 bg-muted/80 backdrop-blur-sm">
            {table.getHeaderGroups().map(headerGroup => (
              <tr key={headerGroup.id}>
                {headerGroup.headers.map(header => {
                  const colConfig = columns.find(c => c.id === header.id)
                  const w = header.id === "_index" ? 50 : getColWidth(header.id, colConfig?.width || 180)
                  return (
                  <th
                    key={header.id}
                    data-col-id={header.id}
                    className="text-left font-normal border-b border-r last:border-r-0 h-8 whitespace-nowrap overflow-hidden relative group/th"
                    style={{ width: w, maxWidth: w }}
                  >
                    {header.isPlaceholder
                      ? null
                      : flexRender(header.column.columnDef.header, header.getContext())}
                    {/* Resize handle */}
                    {header.id !== "_index" && (
                      <div
                        onMouseDown={(e) => handleResizeStart(header.id, e)}
                        className="absolute right-0 top-0 bottom-0 w-1 cursor-col-resize hover:bg-primary/40 active:bg-primary/60 transition-colors"
                      />
                    )}
                  </th>
                  )
                })}
                {/* Add Column Button + Picker */}
                <th className="w-10 border-b relative">
                  <button
                    onClick={() => setShowColPicker(!showColPicker)}
                    className="p-1 rounded hover:bg-muted-foreground/10 transition-colors"
                    title="Add column"
                  >
                    <Plus className="size-3.5 text-muted-foreground" />
                  </button>
                  {showColPicker && (
                    <div className="absolute right-0 top-8 z-50 w-72 rounded-xl border bg-card shadow-xl p-3 space-y-3 animate-in fade-in slide-in-from-top-2 duration-200">
                      <div className="text-xs font-medium text-muted-foreground">Add Column</div>
                      <input
                        autoFocus
                        placeholder="Column name..."
                        value={newColName}
                        onChange={e => setNewColName(e.target.value)}
                        className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
                      />

                      {/* Column Type Picker */}
                      <div className="grid grid-cols-2 gap-1.5">
                        {(["lead_field", "ai_formula", "waterfall", "enrichment"] as const).map(t => {
                          const m = COL_TYPE_META[t]
                          const Icon = m.icon
                          return (
                            <button
                              key={t}
                              onClick={() => setNewColType(t)}
                              className={`flex items-center gap-1.5 px-2 py-1.5 rounded-md text-xs transition-colors ${
                                newColType === t ? "bg-primary/10 border border-primary/30" : "hover:bg-muted border border-transparent"
                              }`}
                            >
                              <Icon className={`size-3.5 ${m.color}`} />
                              {m.label}
                            </button>
                          )
                        })}
                      </div>

                      {/* Lead field selector (for lead_field type) */}
                      {newColType === "lead_field" && (
                        <div className="space-y-1.5">
                          <label className="text-[11px] text-muted-foreground">Map to Lead field</label>
                          <select
                            value={newColLeadField}
                            onChange={e => setNewColLeadField(e.target.value)}
                            className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
                          >
                            <option value="">Select field...</option>
                            {["company", "website", "email", "phone",
                              "contact_person", "contact_title", "decision_makers",
                              "city", "state", "address",
                              "specialization", "company_size", "employee_count_exact",
                              "description", "revenue_range", "founded_year",
                              "industry_tags", "technologies", "funding_stage",
                              "linkedin_url", "twitter_url", "facebook_url",
                              "secondary_emails", "secondary_phones",
                              "glassdoor_rating", "hiring_signals",
                              "score", "score_tier", "status", "source",
                              "yupcha_value_prop", "company_need", "notes",
                              "created_at", "updated_at", "last_enriched_at"].map(f => (
                              <option key={f} value={f}>{f.replace(/_/g, " ")}</option>
                            ))}
                          </select>
                        </div>
                      )}

                      {/* AI Prompt (only for ai_formula) */}
                      {newColType === "ai_formula" && (
                        <div className="space-y-1.5">
                          <label className="text-[11px] text-muted-foreground">
                            AI Prompt — use {"{column_name}"} for placeholders
                          </label>
                          <textarea
                            value={newColPrompt}
                            onChange={e => setNewColPrompt(e.target.value)}
                            placeholder="Summarize what {company} does based on {website}"
                            rows={3}
                            className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50 resize-none"
                          />
                        </div>
                      )}

                      {/* Condition (for enrichment/waterfall/ai) */}
                      {newColType !== "lead_field" && (
                        <div className="space-y-1.5">
                          <label className="text-[11px] text-muted-foreground">Only Run If (optional)</label>
                          <input
                            value={newColCondition}
                            onChange={e => setNewColCondition(e.target.value)}
                            placeholder='{email} == "" AND {website} != ""'
                            className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono focus:outline-none focus:ring-1 focus:ring-primary/50"
                          />
                        </div>
                      )}

                      {/* Actions */}
                      <div className="flex gap-2 justify-end pt-1">
                        <button
                          onClick={() => { setShowColPicker(false); setNewColName(""); setNewColPrompt(""); setNewColCondition(""); setNewColLeadField("") }}
                          className="px-2.5 py-1 text-xs rounded-md hover:bg-muted transition-colors"
                        >
                          Cancel
                        </button>
                        <button
                          onClick={() => {
                            if (!newColName.trim()) return
                            const colId = newColName.toLowerCase().replace(/\s+/g, "_")
                            const newCol: any = {
                              id: colId,
                              name: newColName.trim(),
                              type: newColType,
                              width: newColType === "ai_formula" ? 300 : 200,
                            }
                            if (newColType === "lead_field" && newColLeadField) {
                              newCol.lead_field = newColLeadField
                            }
                            if (newColType === "ai_formula" && newColPrompt.trim()) {
                              newCol.prompt = newColPrompt.trim()
                            }
                            if (newColCondition.trim()) {
                              newCol.condition = newColCondition.trim()
                            }
                            updateWb.mutate({
                              id: workbook.id,
                              columns_config: [...columns, newCol] as any,
                            })
                            setShowColPicker(false)
                            setNewColName("")
                            setNewColPrompt("")
                            setNewColCondition("")
                            setNewColLeadField("")
                            setNewColType("lead_field")
                          }}
                          disabled={!newColName.trim()}
                          className="px-2.5 py-1 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50 transition-colors"
                        >
                          Add Column
                        </button>
                      </div>
                    </div>
                  )}
                </th>
              </tr>
            ))}
          </thead>

          <tbody>
            {/* Virtual padding top */}
            {getVirtualItems().length > 0 && getVirtualItems()[0].start > 0 && (
              <tr><td style={{ height: getVirtualItems()[0].start }} /></tr>
            )}

            {getVirtualItems().map(virtualRow => {
              const row = table.getRowModel().rows[virtualRow.index]
              if (!row) return null
              return (
                <tr
                  key={row.id}
                  className="group hover:bg-muted/30 transition-colors"
                >
                  {row.getVisibleCells().map(cell => {
                    const colConfig = columns.find(c => c.id === cell.column.id)
                    const w = cell.column.id === "_index" ? 50 : getColWidth(cell.column.id, colConfig?.width || 180)
                    return (
                    <td
                      key={cell.id}
                      data-col-id={cell.column.id}
                      className="border-b border-r last:border-r-0 h-9 p-0 overflow-hidden"
                      style={{ width: w, maxWidth: w }}
                    >
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                    )
                  })}
                  <td className="border-b w-10 p-0" />
                </tr>
              )
            })}

            {/* Virtual padding bottom */}
            {getVirtualItems().length > 0 && (
              <tr>
                <td style={{
                  height: getTotalSize() - (getVirtualItems()[getVirtualItems().length - 1]?.end ?? 0),
                }} />
              </tr>
            )}
          </tbody>
        </table>

        {/* Empty State */}
        {rows.length === 0 && (
          <div className="flex flex-col items-center justify-center py-20 text-center">
            <FileSpreadsheet className="size-10 text-muted-foreground/30 mb-4" />
            <p className="text-sm text-muted-foreground mb-2">No leads match your filter</p>
            <p className="text-xs text-muted-foreground/70 mb-4 max-w-xs">
              {workbook.filter_criteria && Object.keys(workbook.filter_criteria).length > 0
                ? "Try adjusting the filter criteria or import leads via CSV"
                : "Import a CSV file to add leads to your database"}
            </p>
            <button
              onClick={() => fileInputRef.current?.click()}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs bg-primary text-primary-foreground hover:bg-primary/90 transition-colors"
            >
              <Upload className="size-3.5" />
              Import CSV
            </button>
          </div>
        )}
      </div>

      {/* ── Column Context Menu ─────────────────────────────────────── */}
      {ctxMenu && (() => {
        const col = columns.find(c => c.id === ctxMenu.colId)
        if (!col) return null
        const tableCol = table.getColumn(ctxMenu.colId)
        return (
          <div
            className="fixed z-[100] min-w-[160px] rounded-lg border bg-card shadow-xl py-1 animate-in fade-in zoom-in-95 duration-150"
            style={{ left: ctxMenu.x, top: ctxMenu.y }}
            onClick={e => e.stopPropagation()}
          >
            <div className="px-3 py-1.5 text-[10px] font-medium text-muted-foreground uppercase tracking-wide">{col.name}</div>
            <button
              onClick={() => { tableCol?.toggleSorting(false); setCtxMenu(null) }}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted transition-colors"
            >
              <ArrowUp className="size-3.5" /> Sort Ascending
            </button>
            <button
              onClick={() => { tableCol?.toggleSorting(true); setCtxMenu(null) }}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted transition-colors"
            >
              <ArrowDown className="size-3.5" /> Sort Descending
            </button>
            <div className="h-px bg-border mx-2 my-1" />
            <button
              onClick={() => handleHideColumn(ctxMenu.colId)}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted transition-colors"
            >
              <EyeOff className="size-3.5" /> Hide Column
            </button>
            <div className="h-px bg-border mx-2 my-1" />
            <button
              onClick={() => handleDeleteColumn(ctxMenu.colId)}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs text-destructive hover:bg-destructive/10 transition-colors"
            >
              <Trash2 className="size-3.5" /> Delete Column
            </button>
          </div>
        )
      })()}

      {/* ── Status Bar ───────────────────────────────────────────────── */}
      <div className="flex items-center justify-between px-4 py-1 border-t text-[11px] text-muted-foreground bg-muted/30 shrink-0">
        <div className="flex items-center gap-3">
          <span className="tabular-nums">{rows.length} of {data?.total_rows ?? 0} rows</span>
          <span className="text-border">│</span>
          <span>{columns.length} columns</span>
          {columns.filter(c => c.type === "waterfall" || c.type === "enrichment").length > 0 && (
            <>
              <span className="text-border">│</span>
              <span>{columns.filter(c => c.type === "waterfall" || c.type === "enrichment").length} enrichment</span>
            </>
          )}
        </div>
        <div className="flex items-center gap-3">
          {connected && (
            <span className="inline-flex items-center gap-1 text-emerald-400">
              <span className="size-1.5 rounded-full bg-emerald-400" />
              Live
            </span>
          )}
          {workbook?.status === "running" && (() => {
            // Compute enrichment stats from row overlays
            const enrichCols = columns.filter(c => c.type === "waterfall" || c.type === "enrichment" || c.type === "ai_formula")
            const totalCells = rows.length * enrichCols.length
            let complete = 0, errors = 0, running = 0, pending = 0
            for (const row of rows) {
              for (const col of enrichCols) {
                const s = row.enrichments?.[col.id]?.status
                if (s === "complete") complete++
                else if (s === "error" || s === "skipped") errors++
                else if (s === "running") running++
                else pending++
              }
            }
            const pct = totalCells > 0 ? Math.round(((complete + errors) / totalCells) * 100) : 0
            return (
              <span className="inline-flex items-center gap-2 text-blue-400">
                <Loader2 className="size-3 animate-spin" />
                <span className="tabular-nums">{complete}/{totalCells} cells</span>
                {errors > 0 && <span className="text-red-400 tabular-nums">{errors} err</span>}
                {running > 0 && <span className="text-amber-400 tabular-nums">{running} active</span>}
                <span className="inline-flex items-center gap-1">
                  <span className="w-16 h-1.5 rounded-full bg-muted overflow-hidden">
                    <span className="h-full rounded-full bg-blue-400 transition-all duration-300" style={{ width: `${pct}%` }} />
                  </span>
                  <span className="tabular-nums">{pct}%</span>
                </span>
              </span>
            )
          })()}
        </div>
      </div>
    </div>
  )
}
