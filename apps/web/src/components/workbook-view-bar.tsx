/**
 * Workbook saved views — toolbar switcher + filter builder.
 *
 * Views are named presets of {filters, sort, hidden_columns} persisted per
 * workbook (backend: /api/v2/workbooks/{id}/views). They are presentation-layer
 * only: filters and sorting are applied before server pagination. Switching
 * views refetches scoped rows and never mutates their data.
 */

import { useEffect, useMemo, useRef, useState } from "react"
import { AlertDialog, Button, Dialog, Input, Menu } from "@/design-system/primitives"
import {
  Check, ChevronDown, Eye, Filter as FilterIcon, Layers3, Pencil, Plus, Save, Trash2, X,
} from "lucide-react"
import { NativeSelect } from "@/components/ui/native-select"
import { toast } from "sonner"
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

// Local names keep the view markup readable while using Twenty's menu system.
const DropdownMenu = Menu.Root
const DropdownMenuContent = Menu.Popup
const DropdownMenuGroup = Menu.Group
const DropdownMenuItem = Menu.Item
const DropdownMenuLabel = Menu.GroupLabel
const DropdownMenuSeparator = Menu.Separator
const DropdownMenuTrigger = Menu.Trigger

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
  const viewQuery = useWorkbookViews(workbookId)
  const { data } = viewQuery
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
  const namingPending = useRef(false)
  const [nameError, setNameError] = useState<string | null>(null)
  const [filterError, setFilterError] = useState<string | null>(null)
  const filterPending = useRef(false)
  const [deleteTarget, setDeleteTarget] = useState<WorkbookView | null>(null)
  const [deleteError, setDeleteError] = useState<string | null>(null)
  const deleting = useRef(false)
  const viewTrigger = useRef<HTMLButtonElement>(null)

  // Keep the filter draft in sync with the active view.
  useEffect(() => {
    if (filterOpen) return
    setDraftFilters(activeView?.config?.filters ?? [])
  }, [activeViewId, activeView?.config, filterOpen])

  const currentConfig = (filters: ViewFilter[]): ViewConfig => ({
    filters,
    sort: sortingStateToSort(sorting),
    hidden_columns: [...hiddenColumns],
  })

  const handleCreate = () => {
    const name = createName.trim()
    if (!name || namingPending.current) return
    namingPending.current = true
    setNameError(null)
    createMut.mutate(
      { name, config: currentConfig(activeView ? draftFilters : []) },
      {
        onSuccess: (v) => {
          setCreateOpen(false)
          setCreateName("")
          onSelectView(v)
          toast.success(`View "${v.name}" created`)
        },
        onError: () => setNameError("Could not create the view. Your name is still here; try again."),
        onSettled: () => { namingPending.current = false },
      },
    )
  }

  const handleRename = () => {
    const name = renameName.trim()
    if (!name || !activeView || namingPending.current) return
    namingPending.current = true
    setNameError(null)
    updateMut.mutate(
      { viewId: activeView.id, name },
      {
        onSuccess: () => { setRenameOpen(false); toast.success("View renamed") },
        onError: () => setNameError("Could not rename the view. Your name is still here; try again."),
        onSettled: () => { namingPending.current = false },
      },
    )
  }

  const handleDelete = () => {
    if (!deleteTarget || deleting.current) return
    deleting.current = true
    setDeleteError(null)
    deleteMut.mutate(deleteTarget.id, {
      onSuccess: () => { setDeleteTarget(null); onSelectView(null); toast.success("View deleted") },
      onError: () => setDeleteError("Deletion was not confirmed. Refresh the view list before retrying if the connection was interrupted."),
      onSettled: () => { deleting.current = false },
    })
  }

  /** Persist filters draft + the CURRENT sort/hidden-columns into the view. */
  const handleSaveConfig = (filters: ViewFilter[]) => {
    if (!activeView || filterPending.current) return
    filterPending.current = true
    setFilterError(null)
    updateMut.mutate(
      { viewId: activeView.id, config: currentConfig(filters) },
      {
        onSuccess: () => { setFilterOpen(false); toast.success("View saved") },
        onError: () => { setFilterError("Could not save filters. Your draft is still here; try again."); toast.error("Failed to save view") },
        onSettled: () => { filterPending.current = false },
      },
    )
  }

  const filterCount = activeView?.config?.filters?.length ?? 0

  return (
    <div className="flex items-center gap-1">
      {/* ── View switcher ── */}
      <DropdownMenu>
        <DropdownMenuTrigger ref={viewTrigger} className="inline-flex items-center gap-1.5 px-2.5 py-1.5 rounded-md border bg-background text-xs font-medium hover:bg-accent transition-colors max-w-44">
          <Layers3 className="size-3.5 text-muted-foreground shrink-0" />
          <span className="truncate">{activeView ? activeView.name : activeViewId ? "Selected view unavailable" : "All rows"}</span>
          <ChevronDown className="size-3 text-muted-foreground shrink-0" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-56">
          <DropdownMenuGroup>
            <DropdownMenuLabel className="text-[10px] uppercase tracking-wide text-muted-foreground">Views</DropdownMenuLabel>
            {viewQuery.isPending && <p role="status" className="px-2 py-2 text-sm">Loading saved views…</p>}
            {viewQuery.isError && <p role="alert" className="px-2 py-2 text-sm">Could not load saved views.</p>}
            {viewQuery.isError && <DropdownMenuItem disabled={viewQuery.isFetching} onClick={() => void viewQuery.refetch()}>Retry saved views</DropdownMenuItem>}
            <DropdownMenuItem onClick={() => onSelectView(null)}>
              <Eye className="size-3.5" />
              All rows
              {!activeViewId && <Check aria-hidden="true" className="ml-auto size-3.5 text-foreground" />}
            </DropdownMenuItem>
            {views.map(v => (
              <DropdownMenuItem key={v.id} onClick={() => onSelectView(v)}>
                <Layers3 className="size-3.5" />
                <span className="truncate">{v.name}</span>
                {v.id === activeViewId && <Check aria-hidden="true" className="ml-auto size-3.5 text-foreground" />}
              </DropdownMenuItem>
            ))}
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem disabled={viewQuery.isPending || viewQuery.isError} onClick={() => { setNameError(null); setCreateName(""); setCreateOpen(true) }}>
            <Plus className="size-3.5" /> New view
          </DropdownMenuItem>
          {activeView && (
            <>
              <DropdownMenuItem onClick={() => handleSaveConfig(draftFilters)}>
                <Save className="size-3.5" /> Save sort & columns to view
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => { setNameError(null); setRenameName(activeView.name); setRenameOpen(true) }}>
                <Pencil className="size-3.5" /> Rename view
              </DropdownMenuItem>
              <DropdownMenuItem className="text-destructive" onClick={() => { setDeleteError(null); setDeleteTarget(activeView) }}>
                <Trash2 className="size-3.5" /> Delete view
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>

      {/* ── Filter builder (only meaningful with a view selected) ── */}
      {activeView && (
        <Dialog.Root open={filterOpen} onOpenChange={open => { if (!filterPending.current) { setFilterOpen(open); setFilterError(null) } }}>
          <Dialog.Trigger render={<Button />}
            className={`inline-flex items-center gap-1 px-2 py-1.5 rounded-md border text-xs transition-colors hover:bg-accent ${filterCount > 0 ? "text-primary border-primary/30" : "text-muted-foreground"}`}
            title="Edit this view's filters"
          >
            <FilterIcon className="size-3.5" />
            {filterCount > 0 ? `${filterCount} filter${filterCount > 1 ? "s" : ""}` : "Filter"}
          </Dialog.Trigger>
          <Dialog.Popup size="lg" style={{ width: "min(640px, calc(100vw - 32px))", maxHeight: "calc(100dvh - 32px)" }}>
            <Dialog.Header><Dialog.Title>View filters</Dialog.Title><Dialog.Description>Filters for “{activeView.name}”. All conditions must match.</Dialog.Description></Dialog.Header>
            <Dialog.Body className="space-y-3 overflow-y-auto min-h-0">
            {filterError && <p role="alert" className="text-sm">{filterError}</p>}
            {draftFilters.length === 0 && (
              <p className="text-[11px] text-muted-foreground/70">No filters — this view shows every row.</p>
            )}
            {draftFilters.map((f, i) => (
              <div key={i} className="flex flex-wrap items-center gap-1">
                <NativeSelect
                  aria-label={`Filter ${i + 1} column`}
                  disabled={updateMut.isPending}
                  value={f.column}
                  onChange={e => setDraftFilters(prev => prev.map((x, j) => j === i ? { ...x, column: e.target.value } : x))}
                  className="flex-1 min-w-0"
                >
                  {columns.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
                </NativeSelect>
                <NativeSelect
                  aria-label={`Filter ${i + 1} operator`}
                  disabled={updateMut.isPending}
                  value={f.op}
                  onChange={e => setDraftFilters(prev => prev.map((x, j) => j === i ? { ...x, op: e.target.value as ViewFilterOp } : x))}
                  className="w-32 shrink-0"
                >
                  {(Object.keys(OP_LABELS) as ViewFilterOp[]).map(op => (
                    <option key={op} value={op}>{OP_LABELS[op]}</option>
                  ))}
                </NativeSelect>
                {!NO_VALUE_OPS.includes(f.op) && (
                  <input
                    aria-label={`Filter ${i + 1} value`}
                    disabled={updateMut.isPending}
                    value={String(f.value ?? "")}
                    onChange={e => setDraftFilters(prev => prev.map((x, j) => j === i ? { ...x, value: e.target.value } : x))}
                    placeholder="value"
                    className="w-24 shrink-0 px-1.5 py-1 rounded border bg-background text-xs"
                  />
                )}
                <button
                  aria-label={`Remove filter ${i + 1}`}
                  disabled={updateMut.isPending}
                  onClick={() => setDraftFilters(prev => prev.filter((_, j) => j !== i))}
                  className="p-1 rounded hover:bg-destructive/10 hover:text-destructive shrink-0"
                >
                  <X className="size-3" />
                </button>
              </div>
            ))}
            </Dialog.Body>
            <Dialog.Footer className="flex-wrap">
              <Button
                disabled={!columns.length || updateMut.isPending}
                onClick={() => setDraftFilters(prev => [...prev, { column: columns[0]?.id ?? "", op: "contains", value: "" }])}
                className="inline-flex items-center gap-1 px-2 py-1 rounded text-xs text-muted-foreground hover:bg-muted transition-colors"
              >
                <Plus className="size-3" /> Add filter
              </Button>
              <Dialog.Close render={<Button disabled={updateMut.isPending} />}>Cancel</Dialog.Close>
              <Button
                onClick={() => handleSaveConfig(draftFilters)}
                disabled={updateMut.isPending}
                className="px-2.5 py-1 rounded-md text-xs bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                Apply & save
              </Button>
            </Dialog.Footer>
          </Dialog.Popup>
        </Dialog.Root>
      )}

      <AlertDialog.Root open={!!deleteTarget} onOpenChange={open => { if (!open && !deleting.current) setDeleteTarget(null) }}>
        <AlertDialog.Popup finalFocus={viewTrigger} style={{ width: "min(400px, calc(100vw - 32px))" }}>
          <AlertDialog.Header><AlertDialog.Title>Delete saved view?</AlertDialog.Title>
            <AlertDialog.Description>Delete “{deleteTarget?.name}”? Workbook rows and their data are not deleted.</AlertDialog.Description>
          </AlertDialog.Header>
          {deleteError && <AlertDialog.Body><p role="alert">{deleteError}</p></AlertDialog.Body>}
          <AlertDialog.Footer>
            <AlertDialog.Close render={<Button disabled={deleteMut.isPending} />}>Cancel</AlertDialog.Close>
            <Button color="danger" variant="solid" loading={deleteMut.isPending} onClick={handleDelete}>Delete view</Button>
          </AlertDialog.Footer>
        </AlertDialog.Popup>
      </AlertDialog.Root>

      {/* ── Saved-view naming dialogs ── */}
      <Dialog.Root open={createOpen} onOpenChange={open => { if (!namingPending.current) { setCreateOpen(open); setNameError(null) } }}>
        <Dialog.Popup finalFocus={viewTrigger} style={{ width: "min(400px, calc(100vw - 32px))" }}>
          <Dialog.Header><Dialog.Title>New view</Dialog.Title><Dialog.Description>Save the current sort and hidden columns.</Dialog.Description></Dialog.Header>
          <Dialog.Body>
            <label htmlFor="create-view-name">View name</label>
            <Input id="create-view-name"
              value={createName}
              onChange={e => setCreateName(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter") handleCreate() }}
              placeholder="View name…"
              className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
            />
            {nameError && <p role="alert">{nameError}</p>}
          </Dialog.Body>
          <Dialog.Footer>
              <Button disabled={createMut.isPending} onClick={() => setCreateOpen(false)}>Cancel</Button>
              <Button
                onClick={handleCreate}
                disabled={!createName.trim() || createMut.isPending}
                className="px-2.5 py-1 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                Create
              </Button>
          </Dialog.Footer>
        </Dialog.Popup>
      </Dialog.Root>

      {/* ── Rename view overlay ── */}
      <Dialog.Root open={renameOpen} onOpenChange={open => { if (!namingPending.current) { setRenameOpen(open); setNameError(null) } }}>
        <Dialog.Popup finalFocus={viewTrigger} style={{ width: "min(400px, calc(100vw - 32px))" }}>
          <Dialog.Header><Dialog.Title>Rename view</Dialog.Title><Dialog.Description>Only the view name changes. Rows are unaffected.</Dialog.Description></Dialog.Header>
          <Dialog.Body>
            <label htmlFor="rename-view-name">View name</label>
            <Input id="rename-view-name"
              value={renameName}
              onChange={e => setRenameName(e.target.value)}
              onKeyDown={e => { if (e.key === "Enter") handleRename() }}
              className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
            />
            {nameError && <p role="alert">{nameError}</p>}
          </Dialog.Body>
          <Dialog.Footer>
              <Button disabled={updateMut.isPending} onClick={() => setRenameOpen(false)}>Cancel</Button>
              <Button
                onClick={handleRename}
                disabled={!renameName.trim() || updateMut.isPending}
                className="px-2.5 py-1 text-xs rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                Rename
              </Button>
          </Dialog.Footer>
        </Dialog.Popup>
      </Dialog.Root>
    </div>
  )
}
