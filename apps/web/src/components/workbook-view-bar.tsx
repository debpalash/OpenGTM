/**
 * Workbook saved views — toolbar switcher + filter builder.
 *
 * Views are named presets of {filters, sort, hidden_columns} persisted per
 * workbook (backend: /api/v2/workbooks/{id}/views). They are presentation-layer
 * only: filtering/sorting is applied CLIENT-SIDE by the editor (rows are already
 * loaded client-side), so switching views is instant and never mutates rows.
 */

import { useEffect, useMemo, useState } from "react"
import {
  ChevronDown, Eye, Filter as FilterIcon, Layers3, Pencil, Plus, Save, Trash2, X,
} from "lucide-react"
import { toast } from "sonner"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuGroup, DropdownMenuItem, DropdownMenuLabel,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import {
  useCreateView, useDeleteView, useUpdateView, useWorkbookViews,
} from "@/lib/workbook-hooks"
import type {
  ViewConfig, ViewFilter, ViewFilterOp, ViewSort, WorkbookView, WorkbookLeadRow,
} from "@/lib/workbook-api"

// ── Filter evaluation (client-side) ──────────────────────────────────────

type ColumnLike = { id: string; name: string; type: string; lead_field?: string | null }

/** Resolve a row's display value for a column — same accessor logic the table uses. */
export function cellValueForColumn(row: WorkbookLeadRow, col: ColumnLike): string {
  if (col.type === "lead_field" || col.type === "input") {
    const v = (row.data || row.lead || {})[col.lead_field || col.id]
    return v == null ? "" : String(v)
  }
  const v = row.enrichments?.[col.id]?.value
  if (v == null || v === "") {
    // Same lead-data fallback the cell renderer uses for enriched columns.
    const fb = (row.lead || {})[col.id] ?? (row.lead || {})[col.lead_field || ""]
    return fb == null ? "" : String(fb)
  }
  return typeof v === "string" ? v : JSON.stringify(v)
}

function matches(value: string, f: ViewFilter): boolean {
  const v = value.toLowerCase()
  const q = String(f.value ?? "").toLowerCase()
  switch (f.op) {
    case "equals": return v === q
    case "not_equals": return v !== q
    case "contains": return v.includes(q)
    case "not_contains": return !v.includes(q)
    case "empty": return v.trim() === ""
    case "not_empty": return v.trim() !== ""
    default: return true
  }
}

/** Apply a view's filter clauses (AND semantics) to the loaded rows. */
export function applyViewFilters(
  rows: WorkbookLeadRow[],
  filters: ViewFilter[] | undefined,
  columns: ColumnLike[],
): WorkbookLeadRow[] {
  if (!filters?.length) return rows
  const byId = new Map(columns.map(c => [c.id, c]))
  const active = filters.filter(f => byId.has(f.column))
  if (!active.length) return rows
  return rows.filter(row =>
    active.every(f => matches(cellValueForColumn(row, byId.get(f.column)!), f)),
  )
}

/** View sort config → TanStack SortingState. */
export function sortToSortingState(sort: ViewSort[] | undefined): { id: string; desc: boolean }[] {
  return (sort ?? []).map(s => ({ id: s.column, desc: s.dir === "desc" }))
}

/** TanStack SortingState → view sort config. */
export function sortingStateToSort(sorting: { id: string; desc: boolean }[]): ViewSort[] {
  return sorting.map(s => ({ column: s.id, dir: s.desc ? "desc" : "asc" }))
}

const OP_LABELS: Record<ViewFilterOp, string> = {
  equals: "equals",
  not_equals: "does not equal",
  contains: "contains",
  not_contains: "does not contain",
  empty: "is empty",
  not_empty: "is not empty",
}
const NO_VALUE_OPS: ViewFilterOp[] = ["empty", "not_empty"]

// ── Component ────────────────────────────────────────────────────────────

export function WorkbookViewBar({
  workbookId, columns, activeViewId, onSelectView,
  sorting, hiddenColumns,
}: {
  workbookId: string
  columns: ColumnLike[]
  activeViewId: string | null
  /** Called with the selected view (or null for "All rows"); the editor applies sort + hidden columns. */
  onSelectView: (view: WorkbookView | null) => void
  /** Current table sorting — captured into the view on create/save. */
  sorting: { id: string; desc: boolean }[]
  /** Currently hidden column ids — captured into the view on create/save. */
  hiddenColumns: Set<string>
}) {
  const { data } = useWorkbookViews(workbookId)
  const views = useMemo(() => data?.views ?? [], [data])
  const activeView = views.find(v => v.id === activeViewId) ?? null

  const createMut = useCreateView(workbookId)
  const updateMut = useUpdateView(workbookId)
  const deleteMut = useDeleteView(workbookId)

  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState("")
  const [renameOpen, setRenameOpen] = useState(false)
  const [renameName, setRenameName] = useState("")
  const [filterOpen, setFilterOpen] = useState(false)
  const [draftFilters, setDraftFilters] = useState<ViewFilter[]>([])

  // Keep the filter draft in sync with the active view.
  useEffect(() => {
    setDraftFilters(activeView?.config?.filters ?? [])
  }, [activeViewId, activeView?.config])

  const currentConfig = (filters: ViewFilter[]): ViewConfig => ({
    filters,
    sort: sortingStateToSort(sorting),
    hidden_columns: [...hiddenColumns],
  })

  const handleCreate = () => {
    const name = createName.trim()
    if (!name) return
    createMut.mutate(
      { name, config: currentConfig(activeView ? draftFilters : []) },
      {
        onSuccess: (v) => {
          setCreateOpen(false)
          setCreateName("")
          onSelectView(v)
          toast.success(`View "${v.name}" created`)
        },
        onError: () => toast.error("Failed to create view"),
      },
    )
  }

  const handleRename = () => {
    const name = renameName.trim()
    if (!name || !activeView) return
    updateMut.mutate(
      { viewId: activeView.id, name },
      {
        onSuccess: () => { setRenameOpen(false); toast.success("View renamed") },
        onError: () => toast.error("Failed to rename view"),
      },
    )
  }

  const handleDelete = () => {
    if (!activeView) return
    if (!confirm(`Delete view "${activeView.name}"? (Rows are never affected.)`)) return
    deleteMut.mutate(activeView.id, {
      onSuccess: () => { onSelectView(null); toast.success("View deleted") },
      onError: () => toast.error("Failed to delete view"),
    })
  }

  /** Persist filters draft + the CURRENT sort/hidden-columns into the view. */
  const handleSaveConfig = (filters: ViewFilter[]) => {
    if (!activeView) return
    updateMut.mutate(
      { viewId: activeView.id, config: currentConfig(filters) },
      {
        onSuccess: () => toast.success("View saved"),
        onError: () => toast.error("Failed to save view"),
      },
    )
  }

  const filterCount = activeView?.config?.filters?.length ?? 0

  return (
    <div className="flex items-center gap-1">
      {/* ── View switcher ── */}
      <DropdownMenu>
        <DropdownMenuTrigger className="inline-flex items-center gap-1.5 px-2.5 py-1.5 rounded-md border bg-background text-xs font-medium hover:bg-accent transition-colors max-w-44">
          <Layers3 className="size-3.5 text-muted-foreground shrink-0" />
          <span className="truncate">{activeView ? activeView.name : "All rows"}</span>
          <ChevronDown className="size-3 text-muted-foreground shrink-0" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-56">
          <DropdownMenuGroup>
            <DropdownMenuLabel className="text-[10px] uppercase tracking-wide text-muted-foreground">Views</DropdownMenuLabel>
            <DropdownMenuItem onClick={() => onSelectView(null)}>
              <Eye className="size-3.5" />
              All rows
              {!activeView && <span className="ml-auto text-primary">✓</span>}
            </DropdownMenuItem>
            {views.map(v => (
              <DropdownMenuItem key={v.id} onClick={() => onSelectView(v)}>
                <Layers3 className="size-3.5" />
                <span className="truncate">{v.name}</span>
                {v.id === activeViewId && <span className="ml-auto text-primary">✓</span>}
              </DropdownMenuItem>
            ))}
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem onClick={() => { setCreateName(""); setCreateOpen(true) }}>
            <Plus className="size-3.5" /> New view
          </DropdownMenuItem>
          {activeView && (
            <>
              <DropdownMenuItem onClick={() => handleSaveConfig(draftFilters)}>
                <Save className="size-3.5" /> Save sort & columns to view
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => { setRenameName(activeView.name); setRenameOpen(true) }}>
                <Pencil className="size-3.5" /> Rename view
              </DropdownMenuItem>
              <DropdownMenuItem variant="destructive" onClick={handleDelete}>
                <Trash2 className="size-3.5" /> Delete view
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>

      {/* ── Filter builder (only meaningful with a view selected) ── */}
      {activeView && (
        <Popover open={filterOpen} onOpenChange={setFilterOpen}>
          <PopoverTrigger
            className={`inline-flex items-center gap-1 px-2 py-1.5 rounded-md border text-xs transition-colors hover:bg-accent ${filterCount > 0 ? "text-primary border-primary/30" : "text-muted-foreground"}`}
            title="Edit this view's filters"
          >
            <FilterIcon className="size-3.5" />
            {filterCount > 0 ? `${filterCount} filter${filterCount > 1 ? "s" : ""}` : "Filter"}
          </PopoverTrigger>
          <PopoverContent align="start" className="w-96 p-3 space-y-2">
            <div className="text-xs font-medium text-muted-foreground">
              Filters for “{activeView.name}” <span className="text-muted-foreground/60">(all must match)</span>
            </div>
            {draftFilters.length === 0 && (
              <p className="text-[11px] text-muted-foreground/70">No filters — this view shows every row.</p>
            )}
            {draftFilters.map((f, i) => (
              <div key={i} className="flex items-center gap-1">
                <select
                  value={f.column}
                  onChange={e => setDraftFilters(prev => prev.map((x, j) => j === i ? { ...x, column: e.target.value } : x))}
                  className="flex-1 min-w-0 px-1.5 py-1 rounded border bg-background text-xs"
                >
                  {columns.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
                </select>
                <select
                  value={f.op}
                  onChange={e => setDraftFilters(prev => prev.map((x, j) => j === i ? { ...x, op: e.target.value as ViewFilterOp } : x))}
                  className="w-32 shrink-0 px-1.5 py-1 rounded border bg-background text-xs"
                >
                  {(Object.keys(OP_LABELS) as ViewFilterOp[]).map(op => (
                    <option key={op} value={op}>{OP_LABELS[op]}</option>
                  ))}
                </select>
                {!NO_VALUE_OPS.includes(f.op) && (
                  <input
                    value={String(f.value ?? "")}
                    onChange={e => setDraftFilters(prev => prev.map((x, j) => j === i ? { ...x, value: e.target.value } : x))}
                    placeholder="value"
                    className="w-24 shrink-0 px-1.5 py-1 rounded border bg-background text-xs"
                  />
                )}
                <button
                  onClick={() => setDraftFilters(prev => prev.filter((_, j) => j !== i))}
                  className="p-1 rounded hover:bg-destructive/10 hover:text-destructive shrink-0"
                >
                  <X className="size-3" />
                </button>
              </div>
            ))}
            <div className="flex items-center justify-between pt-1">
              <button
                onClick={() => setDraftFilters(prev => [...prev, { column: columns[0]?.id ?? "", op: "contains", value: "" }])}
                className="inline-flex items-center gap-1 px-2 py-1 rounded text-xs text-muted-foreground hover:bg-muted transition-colors"
              >
                <Plus className="size-3" /> Add filter
              </button>
              <button
                onClick={() => { handleSaveConfig(draftFilters); setFilterOpen(false) }}
                disabled={updateMut.isPending}
                className="px-2.5 py-1 rounded-md text-xs bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                Apply & save
              </button>
            </div>
          </PopoverContent>
        </Popover>
      )}

      {/* ── Create view overlay (same pattern as the editor's rename-column modal) ── */}
      {createOpen && (
        <div className="fixed inset-0 z-[200] flex items-start justify-center pt-32 bg-black/30 backdrop-blur-sm"
          onClick={() => setCreateOpen(false)}>
          <div className="w-72 rounded-xl border bg-card shadow-2xl p-4 space-y-3 animate-in fade-in zoom-in-95 duration-200"
            onClick={e => e.stopPropagation()}>
            <div className="text-xs font-medium text-muted-foreground">New view</div>
            <input
              autoFocus
              value={createName}
              onChange={e => setCreateName(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter") handleCreate(); if (e.key === "Escape") setCreateOpen(false) }}
              placeholder="View name…"
              className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
            />
            <p className="text-[10px] text-muted-foreground/70">Captures the current sort and hidden columns.</p>
            <div className="flex gap-2 justify-end">
              <button onClick={() => setCreateOpen(false)} className="px-2.5 py-1 text-xs rounded-md hover:bg-muted">Cancel</button>
              <button
                onClick={handleCreate}
                disabled={!createName.trim() || createMut.isPending}
                className="px-2.5 py-1 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                Create
              </button>
            </div>
          </div>
        </div>
      )}

      {/* ── Rename view overlay ── */}
      {renameOpen && (
        <div className="fixed inset-0 z-[200] flex items-start justify-center pt-32 bg-black/30 backdrop-blur-sm"
          onClick={() => setRenameOpen(false)}>
          <div className="w-72 rounded-xl border bg-card shadow-2xl p-4 space-y-3 animate-in fade-in zoom-in-95 duration-200"
            onClick={e => e.stopPropagation()}>
            <div className="text-xs font-medium text-muted-foreground">Rename view</div>
            <input
              autoFocus
              value={renameName}
              onChange={e => setRenameName(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter") handleRename(); if (e.key === "Escape") setRenameOpen(false) }}
              className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
            />
            <div className="flex gap-2 justify-end">
              <button onClick={() => setRenameOpen(false)} className="px-2.5 py-1 text-xs rounded-md hover:bg-muted">Cancel</button>
              <button
                onClick={handleRename}
                disabled={!renameName.trim() || updateMut.isPending}
                className="px-2.5 py-1 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                Rename
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
