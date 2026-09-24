/**
 * Workbook Editor — Hybrid model.
 *
 * Rows are leads from the DB. Columns show lead fields + enrichment overlays.
 * Edits to lead_field columns write back to the Lead record.
 * AI/enrichment columns show overlay data.
 */

import { useState, useMemo, useCallback, useRef, useEffect, useDeferredValue } from "react"
import { useParams, useNavigate } from "react-router-dom"
import {
  useReactTable, getCoreRowModel, getSortedRowModel,
  flexRender, type ColumnDef, type CellContext, type SortingState,
} from "@tanstack/react-table"
import { useVirtualizer } from "@tanstack/react-virtual"
import { workbookColumnWindow } from "@/lib/workbook-column-window"
import { loadedCellProgress, isExecutableColumn } from "@/lib/workbook-progress"
import {
  useWorkbook, useUpdateLeadField, useUpdateWorkbookRow, useBulkUpdateWorkbookRows,
  useImportLeads, useRunWorkbook,
  useDeleteWorkbookRows, useDeleteMatchingWorkbookRows, useWorkbookSocket, useProviders,
  useRunCell, useWorkbookViews, useConnectorRuns,
  useSaveWorkbookColumnWidth,
  useSaveWorkbookColumnOrder,
  useSaveWorkbookColumnSettings,
  useAddWorkbookColumn,
} from "@/lib/workbook-hooks"
import type { ColumnConfig, WorkbookLeadRow, EnrichmentOverlay, Provenance, AiColumnPreset, CostInfo, RunCostEstimate, WorkbookView } from "@/lib/workbook-api"
import { exportWorkbookCsv, fetchAiColumnPresets, fetchRunEstimate, fetchWorkbookCost, generateColumn } from "@/lib/workbook-api"
import {
  ArrowLeft, Plus, Download, Upload,
  Sparkles, Type, Layers, Brain, GitBranch, Send, Globe,
  Loader2, X, AlertCircle, Clock,
  FileSpreadsheet, ExternalLink, Filter, Search, Trash2, Copy,
  ArrowUpDown, ArrowUp, ArrowDown, EyeOff, Eye, Pencil, Settings, GripVertical,
  ChevronDown, ChevronUp, Zap, Columns3, Webhook, Calculator,
  DollarSign, RefreshCw, Mail, MailCheck, MailX, Smartphone, Building2, Users,
  ChartColumn, Target, PenLine, Lightbulb, TriangleAlert,
} from "lucide-react"
import { LinkedInIcon } from "@/components/semantic-icons"
import { Button } from "@/components/ui/button"
import { NativeSelect } from "@/components/ui/native-select"
import { WorkbookViewBar, sortToSortingState } from "@/components/workbook-view-bar"
import { WorkbookSelectionBar } from "@/components/workbooks/selection-bar"
import { WorkbookRunReview } from "@/components/workbooks/run-review"
import { WorkbookRunHistory } from "@/components/workbooks/run-history"
import { ColumnWidthEditor } from "@/components/workbooks/column-width-editor"
import { DeleteColumnDialog } from "@/components/workbooks/delete-column-dialog"
import { Dialog, Button as DesignButton, Input as DesignInput } from "@/design-system/primitives"
import "@/components/workbooks/column-settings.css"
import { normalizeWorkbookColumns } from "@/lib/workbook-columns"
import { selectedWorkbookRowIds, type WorkbookDeleteScope } from "@/lib/workbook-selection"
import { ActivityDrawer } from "@/components/activity-drawer"
import type { SourceEnginePanel as SourceEnginePanelType } from "@/components/source-engine-panel"
import { CsvImportDialog, type CsvImportDraft } from "@/components/csv-import-dialog"
import { toast } from "sonner"
import {
  DndContext, closestCenter, KeyboardSensor, PointerSensor, useSensor, useSensors,
  type DragEndEvent,
} from "@dnd-kit/core"
import {
  SortableContext, horizontalListSortingStrategy, useSortable,
} from "@dnd-kit/sortable"
import { CSS } from "@dnd-kit/utilities"

// ── Column Type Metadata ─────────────────────────────────────────────────

const COL_TYPE_META: Record<string, { icon: typeof Type; color: string; label: string; headerBg: string }> = {
  lead_field: { icon: Type, color: "text-zinc-400", label: "Lead Field", headerBg: "" },
  input: { icon: Type, color: "text-zinc-400", label: "Lead Field", headerBg: "" },  // Legacy alias
  source: { icon: Globe, color: "text-muted-foreground", label: "Source", headerBg: "" },
  agent: { icon: Brain, color: "text-muted-foreground", label: "Agent", headerBg: "" },
  enrichment: { icon: Sparkles, color: "text-violet-400", label: "Enrichment", headerBg: "bg-violet-500/5" },
  waterfall: { icon: Layers, color: "text-blue-400", label: "Waterfall", headerBg: "bg-blue-500/5" },
  ai_formula: { icon: Brain, color: "text-amber-400", label: "AI Formula", headerBg: "bg-amber-500/5" },
  conditional: { icon: GitBranch, color: "text-emerald-400", label: "Conditional", headerBg: "bg-emerald-500/5" },
  output: { icon: Send, color: "text-rose-400", label: "Output", headerBg: "bg-rose-500/5" },
  research: { icon: Globe, color: "text-cyan-400", label: "Research", headerBg: "bg-cyan-500/5" },
  http: { icon: Webhook, color: "text-teal-400", label: "HTTP API", headerBg: "bg-teal-500/5" },
  formula: { icon: Calculator, color: "text-orange-400", label: "Formula", headerBg: "bg-orange-500/5" },
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

// ── Favicon helper ─────────────────────────────────────────────────────
function Favicon({ domain }: { domain?: string }) {
  if (!domain) return null
  return (
    <img
      src={`https://www.google.com/s2/favicons?domain=${domain}&sz=16`}
      alt=""
      className="size-4 rounded shrink-0"
      loading="lazy"
      onError={(e) => { (e.target as HTMLImageElement).style.display = 'none' }}
    />
  )
}

// ── Column Progress Bar ────────────────────────────────────────────────
function ColumnProgressBar({ rows, colId }: { rows: WorkbookLeadRow[]; colId: string }) {
  const total = rows.length
  if (total === 0) return null
  const completed = rows.filter(r => {
    const overlay = r.enrichments?.[colId]
    return overlay?.status === 'complete'
  }).length
  const pct = Math.round((completed / total) * 100)
  if (pct === 0) return null
  return (
    <div className="col-progress">
      <div className="col-progress-fill" style={{ width: `${pct}%` }} />
    </div>
  )
}

// ── Per-fact provenance card ──────────────────────────────────────────────

// License → colour chip. Green = openly redistributable, amber = public-record /
// scraped (use-at-own-risk), red = proprietary-api (vendor ToS: do not resell).
const LICENSE_CHIP: Record<string, string> = {
  "CC0-1.0":         "text-green-600 bg-green-500/10 border-green-500/20",
  "CC-BY-4.0":       "text-green-600 bg-green-500/10 border-green-500/20",
  "public-record":   "text-amber-600 bg-amber-500/10 border-amber-500/20",
  "scraped":         "text-amber-600 bg-amber-500/10 border-amber-500/20",
  "proprietary-api": "text-red-600 bg-red-500/10 border-red-500/20",
  "user-provided":   "text-muted-foreground bg-muted border-border",
  "unknown":         "text-muted-foreground bg-muted border-border",
}

// Default staleness threshold (days) when a column has no per-field
// staleness_ttl_days policy. A fact older than this shows a "stale" dot.
const DEFAULT_STALENESS_DAYS = 90

function relativeTime(iso?: string | null): string {
  if (!iso) return ""
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ""
  const secs = Math.max(0, (Date.now() - t) / 1000)
  const d = Math.floor(secs / 86400)
  if (d >= 1) return `${d}d ago`
  const h = Math.floor(secs / 3600)
  if (h >= 1) return `${h}h ago`
  const m = Math.floor(secs / 60)
  if (m >= 1) return `${m}m ago`
  return "just now"
}

function isStale(iso?: string | null, ttlDays = DEFAULT_STALENESS_DAYS): boolean {
  if (!iso) return false
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return false
  return (Date.now() - t) / 86400000 > ttlDays
}

function ProvenanceCard({ prov, ttlDays }: { prov: Provenance; ttlDays?: number }) {
  const chip = LICENSE_CHIP[prov.license] || LICENSE_CHIP.unknown
  const stale = isStale(prov.fetched_at, ttlDays)
  const rel = relativeTime(prov.fetched_at)
  return (
    <div className="absolute z-50 hidden group-hover/cell:block left-0 top-full mt-1 w-56 rounded-md border bg-popover p-2.5 text-xs shadow-md text-popover-foreground">
      <div className="flex items-center justify-between gap-2 mb-1.5">
        <span className="font-medium truncate">{prov.source}</span>
        <span className={`shrink-0 rounded border px-1.5 py-0.5 text-[10px] font-medium ${chip}`}>
          {prov.license}
        </span>
      </div>
      <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
        {prov.confidence != null && (
          <span>conf {Math.round(prov.confidence * 100)}%</span>
        )}
        {rel && (
          <span className="inline-flex items-center gap-1">
            {stale && (
              <span title="Stale — older than the refresh policy"
                className="inline-block size-1.5 rounded-full bg-amber-500" />
            )}
            {rel}
          </span>
        )}
      </div>
    </div>
  )
}

// ── Editable Cell ────────────────────────────────────────────────────────

function VerifyBadge({ verify }: { verify?: string | null }) {
  if (!verify || verify === "unknown") return null
  const meta: Record<string, { icon: typeof MailCheck; cls: string; title: string }> = {
    valid:     { icon: MailCheck,     cls: "text-[var(--t-color-green9)] bg-[var(--t-color-green9)]/10",  title: "Email verified deliverable" },
    catch_all: { icon: TriangleAlert, cls: "text-[var(--t-color-orange9)] bg-[var(--t-color-orange9)]/10", title: "Catch-all domain (risky but usable)" },
    invalid:   { icon: MailX,         cls: "text-destructive bg-destructive/10",                           title: "Email undeliverable" },
  }
  const m = meta[verify]
  if (!m) return null
  const Icon = m.icon
  return (
    <span title={m.title} role="img" aria-label={m.title}
      className={`shrink-0 inline-flex items-center justify-center size-4 rounded-sm ${m.cls}`}>
      <Icon aria-hidden="true" className="size-3" />
    </span>
  )
}

import { ResearchEvidenceInspector } from "@/components/workbooks/research-evidence"
import type { ResearchEvidence } from "@/lib/workbook-api"
import { useQuickLook, type QuickLookField, type QuickLookPayload } from "@/components/quick-look/quick-look"

function EditableCell({
  value, displayOverride, status, provider, error, verify, provenance, staleTtlDays, isEditable, onSave,
  onRerun, rerunning, research, skipped,
}: {
  value: any; status?: string; provider?: string | null; error?: string | null
  displayOverride?: React.ReactNode
  verify?: string | null; provenance?: Provenance | null; staleTtlDays?: number
  isEditable: boolean; onSave: (v: string) => void
  research?: ResearchEvidence | null
  /** Selected providers that never ran for this result, with the reason. */
  skipped?: { provider: string; reason: string }[] | null
  /** Re-run this cell's enrichment (force). Shown as a hover affordance. */
  onRerun?: () => void; rerunning?: boolean
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
          if (e.key === "Escape") {
            const cell = e.currentTarget.closest<HTMLElement>("[data-grid-row]")
            setEditing(false)
            requestAnimationFrame(() => cell?.focus({ preventScroll: true }))
          }
        }}
        className="w-full h-full px-2 py-1 text-sm bg-transparent border-0 focus:outline-none focus:ring-1 focus:ring-primary/50 rounded"
      />
    )
  }

  // Provenance card replaces the plain title tooltip when present.
  const showProvenance = !!provenance && !!displayValue && status === "complete"
  return (
    <div
      data-editable-cell={isEditable ? "true" : undefined}
      className="relative flex items-center gap-1.5 px-2 py-1 h-full min-h-[32px] max-w-full cursor-default group/cell overflow-hidden"
      onDoubleClick={() => isEditable && setEditing(true)}
      title={showProvenance ? undefined : [
        error ? `Error: ${error}` : displayValue || (provider ? `via ${provider}` : undefined),
        skipped?.length ? `Skipped: ${skipped.map(s => `${s.provider} (${s.reason})`).join(", ")}` : undefined,
      ].filter(Boolean).join("\n") || undefined}
    >
      {showProvenance && <ProvenanceCard prov={provenance!} ttlDays={staleTtlDays} />}
      <CellStatus status={status} />
      <span className="truncate text-sm flex-1 min-w-0">
        {displayOverride ?? displayValue}
      </span>
      {displayValue && <VerifyBadge verify={verify} />}
      {research && <ResearchEvidenceInspector evidence={research} />}
      {onRerun && (
        <button
          onClick={(e) => { e.stopPropagation(); if (!rerunning) onRerun() }}
          className={`${rerunning ? "inline-flex" : "hidden group-hover/cell:inline-flex"} p-0.5 rounded hover:bg-muted shrink-0`}
          title="Re-run this cell (force)"
        >
          <RefreshCw className={`size-3 text-muted-foreground ${rerunning ? "animate-spin" : ""}`} />
        </button>
      )}
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

  // URL fields → clickable link with favicon showing domain only
  if (fieldName === "website" || fieldName === "linkedin_url" || fieldName === "twitter_url" || fieldName === "facebook_url") {
    try {
      const url = value.startsWith("http") ? value : `https://${value}`
      const domain = new URL(url).hostname.replace("www.", "")
      const display = fieldName.includes("linkedin") ? domain.replace("linkedin.com/in/", "") 
        : fieldName.includes("twitter") ? domain.replace("twitter.com/", "@")
        : domain
      return (
        <span className="inline-flex items-center gap-1.5 min-w-0">
          <Favicon domain={domain} />
          <a href={url} target="_blank" rel="noopener" onClick={e => e.stopPropagation()}
            className="text-blue-400 hover:text-blue-300 hover:underline truncate text-sm">
            {display}
          </a>
        </span>
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

// ── Sortable Column Header (DnD wrapper) ─────────────────────────────────

function SortableColumnHeader({ id, w, className, children, ...rest }: {
  id: string; w: number; className?: string; children: React.ReactNode;
  [key: string]: any;
}) {
  const {
    attributes,
    listeners,
    setNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id })

  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.5 : 1,
    width: w,
    maxWidth: w,
  }

  return (
    <th
      ref={setNodeRef}
      style={style}
      className={className}
      {...attributes}
      {...listeners}
      {...rest}
    >
      {children}
    </th>
  )
}

// ── Inline cost chip ─────────────────────────────────────────────────────
// Replaces the old blocking window.confirm() spend gate. Always-visible:
// shows spend-to-date (polled live while a run is active) and, on hover, the
// estimated cost of the next run. Click opens the Source Engine cost tab to set
// a budget ceiling. Free OSS providers cost $0, so most runs read "$0.000".
function CostChip({ workbookId, isRunning, viewId, search, onClick }: {
  workbookId: string; isRunning: boolean; viewId?: string | null; search?: string; onClick: () => void
}) {
  const [cost, setCost] = useState<CostInfo | null>(null)
  const [est, setEst] = useState<RunCostEstimate | null>(null)

  useEffect(() => {
    let alive = true
    const load = () => fetchWorkbookCost(workbookId).then(c => { if (alive) setCost(c) }).catch(() => {})
    load()
    setEst(null)
    fetchRunEstimate(workbookId, viewId, search).then(e => { if (alive) setEst(e) }).catch(() => {})
    // Poll spend live during a run so the number climbs as paid providers charge.
    const iv = isRunning ? setInterval(load, 3000) : null
    return () => { alive = false; if (iv) clearInterval(iv) }
  }, [workbookId, isRunning, viewId, search])

  const spent = cost?.budget_spent_usd ?? 0
  const cap = cost?.budget_max_usd ?? 0
  const overCap = cap > 0 && spent >= cap
  const title = [
    `Spent: $${spent.toFixed(3)}`,
    cap > 0 ? `Budget: $${cap.toFixed(2)}` : "Budget: unlimited",
    est ? `Next run est: $${est.best_usd.toFixed(2)}–$${est.worst_usd.toFixed(2)} (${est.rows} rows)` : "",
    "Click to set a budget",
  ].filter(Boolean).join("\n")

  return (
    <button
      onClick={onClick}
      title={title}
      className={`inline-flex items-center gap-1 px-2 py-1.5 rounded-md text-xs tabular-nums transition-colors hover:bg-muted ${overCap ? "text-destructive" : "text-muted-foreground"}`}
    >
      <DollarSign className="size-3.5" />
      <span>{spent.toFixed(3)}</span>
      {cap > 0 && <span className="opacity-60">/ {cap.toFixed(0)}</span>}
    </button>
  )
}

// ── Main Editor Page ─────────────────────────────────────────────────────

export default function WorkbookEditorPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [workbookPage, setWorkbookPage] = useState(1)
  const [activeViewId, setActiveViewId] = useState<string | null>(null)
  const [globalFilter, setGlobalFilter] = useState("")
  const [workbookCursor, setWorkbookCursor] = useState<string | null>(null)
  const [cursorHistory, setCursorHistory] = useState<Array<string | null>>([])
  const deferredGlobalFilter = useDeferredValue(globalFilter)
  const workbookPageSize = 1000
  const cursorMode = !activeViewId
  const { data, isLoading, isFetching, error, refetch } = useWorkbook(
    id!, workbookPageSize, workbookPage, activeViewId, deferredGlobalFilter, cursorMode, workbookCursor,
  )
  const [deleteColumnScope, setDeleteColumnScope] = useState<ColumnConfig | null>(null)
  const { mutate: saveColumnWidth } = useSaveWorkbookColumnWidth(id!)
  const settingsSave = useSaveWorkbookColumnSettings(id!)
  const addColumnMut = useAddWorkbookColumn(id!)
  const addingColumn = useRef(false)
  const persistNewColumn = async (column: Partial<ColumnConfig>) => {
    if (addingColumn.current) return false
    addingColumn.current = true
    try {
      await addColumnMut.mutateAsync(column)
      return true
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to add column")
      return false
    } finally { addingColumn.current = false }
  }
  const renameSave = useSaveWorkbookColumnSettings(id!)
  const renameSubmitting = useRef(false)
  const renameFocus = useRef<{ columnId: string; name: string } | null>(null)
  const settingsSubmitting = useRef(false)
  const saveColumnOrder = useSaveWorkbookColumnOrder(id!)
  const { mutate: updateLeadField } = useUpdateLeadField(id!)
  const { mutate: updateWorkbookRow } = useUpdateWorkbookRow(id!)
  const bulkUpdateWorkbookRows = useBulkUpdateWorkbookRows(id!)
  const importLeadsMut = useImportLeads(id!)
  const runMut = useRunWorkbook(id!)
  const deleteMut = useDeleteWorkbookRows(id!)
  const deleteMatchingMut = useDeleteMatchingWorkbookRows(id!)
  const { mutate: runCell, isPending: cellRunPending, variables: cellRunVariables } = useRunCell(id!)
  const { data: viewsData } = useWorkbookViews(id!)
  const { data: connectorRunsData } = useConnectorRuns(data?.workbook ? id : undefined)
  // Only open the live socket once the workbook has actually loaded — a 404/403
  // workbook should never spawn a doomed WebSocket that just 403s in a loop.
  const { connected } = useWorkbookSocket(data?.workbook ? id : undefined)
  const { data: providersData } = useProviders()
  const tableContainerRef = useRef<HTMLDivElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [csvImportDraft, setCsvImportDraft] = useState<CsvImportDraft | null>(null)
  const [showSourcePanel, setShowSourcePanel] = useState(false)
  const [SourceEnginePanel, setSourceEnginePanel] = useState<typeof SourceEnginePanelType | null>(null)
  const [sourcePanelLoading, setSourcePanelLoading] = useState(false)
  const sourcePanelLoadingRef = useRef(false)
  const openSourcePanel = async () => {
    if (SourceEnginePanel) {
      setShowSourcePanel(true)
      return
    }
    if (sourcePanelLoadingRef.current) return
    sourcePanelLoadingRef.current = true
    setSourcePanelLoading(true)
    try {
      const module = await import("@/components/source-engine-panel")
      setSourceEnginePanel(() => module.SourceEnginePanel)
      setShowSourcePanel(true)
    } catch {
      toast.error("Source Engine could not load. Reload the page and try again.")
    } finally {
      sourcePanelLoadingRef.current = false
      setSourcePanelLoading(false)
    }
  }
  const [showColPicker, setShowColPicker] = useState(false)
  const [aiPresets, setAiPresets] = useState<AiColumnPreset[]>([])
  useEffect(() => { fetchAiColumnPresets().then(d => setAiPresets(d.presets || [])).catch(() => {}) }, [])
  const [newColName, setNewColName] = useState("")
  const [newColType, setNewColType] = useState<"lead_field" | "ai_formula" | "waterfall" | "enrichment" | "output" | "research" | "http" | "formula">("lead_field")
  const [newColHttpUrl, setNewColHttpUrl] = useState("")
  const [newColHttpMethod, setNewColHttpMethod] = useState<"GET" | "POST">("GET")
  const [newColHttpExtract, setNewColHttpExtract] = useState("")
  const [newColHttpBody, setNewColHttpBody] = useState("")
  const [newColFormula, setNewColFormula] = useState("")
  const [newColLeadField, setNewColLeadField] = useState("")
  const [newColPrompt, setNewColPrompt] = useState("")
  const [newColCondition, setNewColCondition] = useState("")
  const [rowSelection, setRowSelection] = useState<Record<string, boolean>>({})
  const [allMatchingSelected, setAllMatchingSelected] = useState(false)
  const [sorting, setSorting] = useState<SortingState>([])
  const [hiddenColumns, setHiddenColumns] = useState<Set<string>>(new Set())
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number; colId: string } | null>(null)
  const [configPanelColId, setConfigPanelColId] = useState<string | null>(null)
  const configTrigger = useRef<HTMLElement | null>(null)
  const [renamingColId, setRenamingColId] = useState<string | null>(null)
  const [renameValue, setRenameValue] = useState("")
  const [renameExpected, setRenameExpected] = useState("")
  const [newColProvider, setNewColProvider] = useState("")
  const [newColWaterfall, setNewColWaterfall] = useState<string[]>([])
  const [newColTargetField, setNewColTargetField] = useState("")
  // Output-column config
  const [newColDest, setNewColDest] = useState<"webhook" | "crm" | "sequencer" | "airtable" | "sheets">("webhook")
  const [newColCrmType, setNewColCrmType] = useState<"hubspot" | "salesforce">("hubspot")
  const [newColAirtableBase, setNewColAirtableBase] = useState("")
  const [newColAirtableTable, setNewColAirtableTable] = useState("")
  const [newColSheetId, setNewColSheetId] = useState("")
  const [newColSheetRange, setNewColSheetRange] = useState("Sheet1")
  const [newColWebhookUrl, setNewColWebhookUrl] = useState("")
  const [newColWebhookBody, setNewColWebhookBody] = useState("")
  const [newColSequenceId, setNewColSequenceId] = useState("")
  const [newColMaxSteps, setNewColMaxSteps] = useState(4)
  // NL → column generator ("Generate with AI")
  const [nlInstruction, setNlInstruction] = useState("")
  const [nlBusy, setNlBusy] = useState(false)
  const [nlExplanation, setNlExplanation] = useState("")
  const [showColumnVisibility, setShowColumnVisibility] = useState(false)
  const [activityOpen, setActivityOpen] = useState(false)
  const [exportingCsv, setExportingCsv] = useState(false)
  const [activeCell, setActiveCell] = useState({ row: 0, column: 0 })
  const [selectionAnchor, setSelectionAnchor] = useState<{ row: number; column: number } | null>(null)
  const availableProviders = providersData?.providers ?? []
  const queryTotalRows = data?.query_total_rows ?? data?.total_rows ?? 0
  const totalPages = Math.max(1, Math.ceil(queryTotalRows / workbookPageSize))
  const displayedWorkbookPage = data?.page ?? workbookPage

  const changeWorkbookPage = useCallback((nextPage: number) => {
    const bounded = Math.max(1, Math.min(nextPage, totalPages))
    if (bounded === workbookPage) return
    if (cursorMode) {
      if (bounded === workbookPage + 1) {
        if (!data?.next_cursor) return
        setCursorHistory(history => [...history, workbookCursor])
        setWorkbookCursor(data.next_cursor)
      } else if (bounded === workbookPage - 1) {
        const previous = cursorHistory[cursorHistory.length - 1] ?? null
        setCursorHistory(history => history.slice(0, -1))
        setWorkbookCursor(previous)
      } else return
    }
    setWorkbookPage(bounded)
    setActiveCell({ row: 0, column: 0 })
    setSelectionAnchor(null)
    tableContainerRef.current?.scrollTo({ top: 0, behavior: "auto" })
  }, [cursorHistory, cursorMode, data?.next_cursor, totalPages, workbookCursor, workbookPage])

  useEffect(() => {
    if (workbookPage > totalPages) {
      setWorkbookPage(1)
      setWorkbookCursor(null)
      setCursorHistory([])
    }
  }, [totalPages, workbookPage])

  // NL → column: call the generator and pre-fill the custom-column form.
  const handleGenerateColumn = async () => {
    if (!nlInstruction.trim() || !workbook || nlBusy) return
    setNlBusy(true)
    setNlExplanation("")
    try {
      const res = await generateColumn(workbook.id, nlInstruction.trim())
      const col = res.column
      setNewColType(res.kind)
      setNewColName(col.name || "")
      setNewColPrompt(col.prompt || "")
      setNewColFormula(col.formula || "")
      setNewColHttpUrl(col.http_url || "")
      setNewColHttpMethod(col.http_method === "POST" ? "POST" : "GET")
      setNewColHttpExtract(col.http_extract || "")
      setNewColHttpBody(col.http_body == null ? "" : typeof col.http_body === "string" ? col.http_body : JSON.stringify(col.http_body))
      setNlExplanation(res.explanation)
    } catch (e: any) {
      toast.error(e?.message || "Failed to generate column")
    } finally {
      setNlBusy(false)
    }
  }


  // Column resize state — uses a ref to avoid re-rendering entire table on drag
  const columnWidthsRef = useRef<Record<string, number>>({})
  const [, forceResizeRender] = useState(0) // only triggers on mouseUp
  const resizeRef = useRef<{ colId: string; startX: number; startW: number } | null>(null)

  const workbook = data?.workbook
  const rows = data?.rows ?? []
  // Normalize columns — ensure every column has an `id` (templates use `key`)
  const columns = useMemo(() => normalizeWorkbookColumns(workbook?.columns_config ?? []), [workbook?.columns_config])
  useEffect(() => {
    const target = renameFocus.current
    if (!target || renamingColId || !columns.some(column => column.id === target.columnId && column.name === target.name)) return
    const frame = requestAnimationFrame(() => {
      const header = Array.from(tableContainerRef.current?.querySelectorAll<HTMLElement>("th[data-col-id]") || [])
        .find(element => element.dataset.colId === target.columnId)
      ;(header?.querySelector<HTMLElement>("[data-column-settings-trigger]") || tableContainerRef.current)?.focus()
      renameFocus.current = null
    })
    return () => cancelAnimationFrame(frame)
  }, [columns, renamingColId])

  // ── Saved views: backend applies filters/sort before pagination ──
  const views = viewsData?.views ?? []
  const activeView = views.find(v => v.id === activeViewId) ?? null

  const handleSelectView = useCallback((v: WorkbookView | null) => {
    setActiveViewId(v?.id ?? null)
    setWorkbookPage(1)
    setWorkbookCursor(null)
    setCursorHistory([])
    setAllMatchingSelected(false)
    setRowSelection({})
    setSorting(v ? sortToSortingState(v.config?.sort) : [])
    setHiddenColumns(new Set(v?.config?.hidden_columns ?? []))
  }, [])

  const viewRows = rows

  // ── DnD sensors for column reorder ──
  const dndSensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: (event, args) => {
      if (event.code !== "ArrowLeft" && event.code !== "ArrowRight") return undefined
      event.preventDefault()
      const { active, over, collisionRect, droppableRects } = args.context
      if (!active || !collisionRect) return undefined
      const visible = columns.filter(column => !hiddenColumns.has(column.id))
      const index = visible.findIndex(column => column.id === (over?.id ?? active.id))
      if (index < 0) return undefined
      const target = visible[index + (event.code === "ArrowRight" ? 1 : -1)]
      const rect = target && droppableRects.get(target.id)
      if (!rect) return undefined
      // Center-based collision needs center-aligned keyboard coordinates. The
      // sortable default aligns edges and can stay over a much wider origin.
      return { x: rect.left + (rect.width - collisionRect.width) / 2, y: collisionRect.top }
    } }),
  )

  const handleColumnDragEnd = useCallback((event: DragEndEvent) => {
    const { active, over } = event
    if (!over || active.id === over.id || saveColumnOrder.isPending) return

    const oldIndex = columns.findIndex(c => c.id === active.id)
    const newIndex = columns.findIndex(c => c.id === over.id)
    if (oldIndex === -1 || newIndex === -1) return

    const reordered = [...columns]
    const [moved] = reordered.splice(oldIndex, 1)
    reordered.splice(newIndex, 0, moved)

    saveColumnOrder.mutate({ columnIds: reordered.map(column => column.id), expectedColumnIds: columns.map(column => column.id) }, {
      onError: error => toast.error(error.message || "Column order was not saved. Refresh before retrying."),
    })
  }, [columns, saveColumnOrder])

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
  const handleResizeKey = useCallback((colId: string, currentWidth: number, e: React.KeyboardEvent) => {
    // Never forward Space/arrows to the parent sortable-header sensor.
    e.stopPropagation()
    const step = e.shiftKey ? 50 : 10
    const next = e.key === "ArrowRight" ? currentWidth + step
      : e.key === "ArrowLeft" ? currentWidth - step
      : e.key === "Home" ? 80
      : e.key === "End" ? 600 : null
    if (next === null) return
    e.preventDefault()
    const width = Math.max(80, Math.min(600, Math.round(next)))
    if (width === currentWidth) return
    columnWidthsRef.current[colId] = width
    forceResizeRender(n => n + 1)
    saveColumnWidth({ columnId: colId, width }, {
      onError: () => toast.error("Column width was not saved. Resize again to retry; reload may restore the old width."),
    })
  }, [saveColumnWidth])

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
      const resized = resizeRef.current
      resizeRef.current = null
      document.removeEventListener("mousemove", onMouseMove)
      document.removeEventListener("mouseup", onMouseUp)
      document.body.style.cursor = ""
      document.body.style.userSelect = ""
      forceResizeRender(n => n + 1) // single re-render on release
      if (resized) {
        const width = Math.round(columnWidthsRef.current[resized.colId] || resized.startW)
        columnWidthsRef.current[resized.colId] = width
        if (width !== resized.startW) saveColumnWidth({ columnId: resized.colId, width }, {
          onError: () => toast.error("Column width was not saved. Resize again to retry; reload may restore the old width."),
        })
      }
    }
    document.addEventListener("mousemove", onMouseMove)
    document.addEventListener("mouseup", onMouseUp)
    document.body.style.cursor = "col-resize"
    document.body.style.userSelect = "none"
  }, [saveColumnWidth])

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
              aria-label="Select all rows on this page"
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
              aria-label={`Select row ${row.original.row_id ?? row.original.lead_id}`}
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
            {row.original.lead_id != null && (
              <a
                href={`/leads/${row.original.lead_id}`}
                className="opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-foreground transition-all"
                title="Open lead detail"
                onClick={e => e.stopPropagation()}
              >
                <ExternalLink className="size-3" />
              </a>
            )}
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
                        return (row.data || row.lead)[col.lead_field || col.id] ?? ""
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
                <GripVertical className="size-3 text-muted-foreground/20 shrink-0 opacity-0 group-hover/th:opacity-100 transition-opacity cursor-grab active:cursor-grabbing" />
                <Icon className={`size-3 ${meta.color} shrink-0`} />
                <span className="truncate text-xs font-medium flex-1">{col.name}</span>
                {sort === "asc" && <ArrowUp className="size-3 text-primary shrink-0" />}
                {sort === "desc" && <ArrowDown className="size-3 text-primary shrink-0" />}
                {!sort && <ArrowUpDown className="size-3 text-muted-foreground/30 shrink-0 opacity-0 group-hover/th:opacity-100 transition-opacity" />}
                <button type="button" data-column-settings-trigger aria-label={`Configure ${col.name} column`}
                  className="shrink-0 p-1 rounded hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring"
                  onPointerDown={event => event.stopPropagation()}
                  onKeyDown={event => event.stopPropagation()}
                  onClick={event => { event.stopPropagation(); configTrigger.current = event.currentTarget; setConfigPanelColId(col.id) }}>
                  <Settings aria-hidden="true" className="size-3" />
                </button>
              </div>
            )
          },
          cell: ({ row }: CellContext<WorkbookLeadRow, unknown>) => {
            // For lead_field columns (or legacy "input" type), get value from lead data
            if (col.type === "lead_field" || col.type === "input") {
              const leadField = col.lead_field || col.id
              const value = (row.original.data || row.original.lead)[leadField] ?? ""
              const strVal = String(value)

              return (
                <EditableCell
                  value={value}
                  displayOverride={strVal && TYPED_FIELDS.has(leadField)
                    ? <TypedCellValue value={strVal} fieldName={leadField} />
                    : undefined}
                  isEditable={true}
                  onSave={(v) => {
                    if (row.original.row_id != null) {
                      updateWorkbookRow({
                        rowId: row.original.row_id,
                        fields: { [leadField]: v },
                      })
                    } else if (row.original.lead_id != null) {
                      updateLeadField({
                        leadId: row.original.lead_id,
                        fields: { [leadField]: v },
                      })
                    }
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
            const hasOverlayValue = overlay.value != null && overlay.value !== ""
            const sourceData = row.original.data || row.original.lead
            const leadFallback = hasOverlayValue ? null : (sourceData[col.id] ?? sourceData[col.lead_field || ""] ?? null)
            const hasLeadFallback = leadFallback != null && leadFallback !== ""
            const displayValue = hasOverlayValue ? overlay.value : (hasLeadFallback ? String(leadFallback) : null)
            const displayStatus = hasOverlayValue ? overlay.status : (hasLeadFallback ? "complete" : overlay.status)

            // Per-cell force re-run (hover affordance). Output columns are
            // run-once + side-effecting, so force re-pushing needs a confirm.
            const cellRowId = row.original.row_id ?? row.original.lead_id
            const isRerunningCell = cellRunPending
              && cellRunVariables?.rowId === cellRowId
              && cellRunVariables?.colId === col.id
            const handleRerun = () => {
              if (cellRowId == null) {
                toast.error("This row has no workbook identity")
                return
              }
              if (col.type === "output") {
                if (!confirm(`"${col.name}" is an output column (runs once per row). Force re-running will push this row to the destination AGAIN. Continue?`)) return
              }
              runCell({ rowId: cellRowId, colId: col.id, force: true }, {
                onSuccess: (res) => {
                  if (res.status === "complete") toast.success("Cell re-run complete")
                  else if (res.status === "skipped") toast.info("Skipped — output cell already ran (use force)")
                  else toast.error(res.error ? `Cell failed: ${res.error}` : "Cell failed")
                },
                onError: () => toast.error("Failed to re-run cell"),
              })
            }

            return (
              <EditableCell
                value={displayValue}
                status={displayStatus}
                provider={hasOverlayValue ? overlay.provider : (hasLeadFallback ? "lead" : null)}
                error={hasLeadFallback ? null : overlay.error}
                verify={hasOverlayValue ? overlay.verify_status : null}
                provenance={hasOverlayValue ? overlay.provenance : null}
                research={overlay.research}
                skipped={Array.isArray(overlay.skipped_providers) ? overlay.skipped_providers : null}
                isEditable={false}
                onSave={() => {}}
                onRerun={handleRerun}
                rerunning={isRerunningCell}
              />
            )
          },
        } satisfies ColumnDef<WorkbookLeadRow>
      }),
    ]
    return cols
  }, [columns, hiddenColumns, updateLeadField, updateWorkbookRow, runCell, cellRunPending, cellRunVariables])

  const table = useReactTable({
    data: viewRows,
    columns: tableColumns,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    // Search and saved-view filters are already applied before server pagination.
    manualFiltering: true,
    state: { rowSelection, sorting, globalFilter },
    onRowSelectionChange: updater => { setAllMatchingSelected(false); setRowSelection(updater) },
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
    getRowId: (row) => row.row_id != null ? `row:${row.row_id}` : `lead:${row.lead_id}`,
  })

  const selectedCount = Object.keys(rowSelection).filter(k => rowSelection[k]).length

  const handleExportSelected = useCallback(async () => {
    if (!workbook || exportingCsv) return
    setExportingCsv(true)
    try {
      const rowIds = selectedWorkbookRowIds(rowSelection)
      if (!rowIds.length) return
      const blob = await exportWorkbookCsv(workbook.id, activeViewId, deferredGlobalFilter, rowIds)
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = `${workbook.name || "export"}_selected.csv`
      a.click()
      URL.revokeObjectURL(url)
      toast.success(`Exported ${rowIds.length} selected rows`)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Selected CSV export failed")
    } finally {
      setExportingCsv(false)
    }
  }, [activeViewId, deferredGlobalFilter, exportingCsv, rowSelection, workbook])

  const handleDeleteSelected = async (scope: WorkbookDeleteScope) => {
    const result = scope.kind === "matching"
      ? await deleteMatchingMut.mutateAsync({ expected_count: scope.count, confirmation: `DELETE ${scope.count} ROWS`, view_id: scope.viewId || undefined, search: scope.search.trim() || undefined })
      : await deleteMut.mutateAsync({ rowIds: scope.rowIds, leadIds: [] })
    toast.success(`Deleted ${result.deleted} rows`)
    setAllMatchingSelected(false)
    setRowSelection({})
  }

  const handleDeleteColumn = useCallback((colId: string) => {
    // Confirm raw stored configuration, not synthesized display defaults.
    const col = workbook?.columns_config.find(c => c.id === colId)
    if (!col) return
    setDeleteColumnScope(structuredClone(col))
    setCtxMenu(null)
    setConfigPanelColId(null)
  }, [workbook])

  const handleHideColumn = useCallback((colId: string) => {
    setHiddenColumns(prev => new Set([...prev, colId]))
    setCtxMenu(null)
  }, [])

  const handleRenameColumn = useCallback((colId: string) => {
    const col = columns.find(c => c.id === colId)
    if (!col) return
    setRenamingColId(colId)
    setRenameValue(col.name)
    setRenameExpected(col.name)
    renameSave.reset()
    setCtxMenu(null)
  }, [columns, renameSave])

  const handleRenameSubmit = async () => {
    if (!renamingColId || !renameValue.trim() || renameSubmitting.current) return
    renameSubmitting.current = true
    try {
      await renameSave.mutateAsync({ columnId: renamingColId, changes: { name: renameValue.trim() }, expected: { name: renameExpected } })
      renameFocus.current = { columnId: renamingColId, name: renameValue.trim() }
      setRenamingColId(null)
      setRenameValue("")
    } catch { /* The dialog keeps the draft and displays the mutation error. */ }
    finally { renameSubmitting.current = false }
  }

  const handleRunSingleColumn = useCallback((colId: string) => {
    runMut.mutate({ column_ids: [colId] }, {
      onSuccess: (data) => toast.success(data.message),
      onError: () => toast.error("Failed to start enrichment"),
    })
    setCtxMenu(null)
  }, [runMut])

  const handleForceRunColumn = useCallback((colId: string) => {
    const col = columns.find(c => c.id === colId)
    if (!col) return
    // Output columns are run-once + side-effecting: force re-pushes EVERY row.
    if (col.type === "output"
        && !confirm(`"${col.name}" is an output column (runs once per row). Force re-running will push EVERY row to the destination again. Continue?`)) {
      setCtxMenu(null)
      return
    }
    runMut.mutate({ column_ids: [colId], force: true }, {
      onSuccess: (data) => toast.success(data.message),
      onError: () => toast.error("Failed to start force re-run"),
    })
    setCtxMenu(null)
  }, [columns, runMut])

  // Close context menu on click outside
  useEffect(() => {
    if (!ctxMenu) return
    const close = () => setCtxMenu(null)
    window.addEventListener("click", close)
    return () => window.removeEventListener("click", close)
  }, [ctxMenu])

  // ── Virtual scrolling ──────────────────────────────────────────────────

  const rowVirtualizer = useVirtualizer({
    count: table.getRowModel().rows.length,
    getScrollElement: () => tableContainerRef.current,
    estimateSize: () => 36,
    overscan: 20,
  })
  const { getVirtualItems, getTotalSize } = rowVirtualizer
  const [horizontalViewport, setHorizontalViewport] = useState({ left: 0, width: 1440 })
  useEffect(() => {
    const container = tableContainerRef.current
    if (!container) return
    let frame = 0
    const measure = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => setHorizontalViewport(previous => {
        const next = { left: container.scrollLeft, width: container.clientWidth }
        return previous.left === next.left && previous.width === next.width ? previous : next
      }))
    }
    const observer = new ResizeObserver(measure)
    observer.observe(container)
    container.addEventListener("scroll", measure, { passive: true })
    measure()
    return () => { observer.disconnect(); container.removeEventListener("scroll", measure); cancelAnimationFrame(frame) }
  }, [workbook?.id, isLoading])
  const visibleGridColumns = table.getVisibleLeafColumns()
  const gridColumnWidths = visibleGridColumns.map(column => column.id === "_select" ? 36 : column.id === "_index" ? 50 : getColWidth(column.id, columns.find(config => config.id === column.id)?.width || 180))
  const columnWindow = workbookColumnWindow(gridColumnWidths, horizontalViewport.left, horizontalViewport.width, [0, 1, activeCell.column])

  const focusGridCell = useCallback((row: number, column: number) => {
    const rowCount = table.getRowModel().rows.length
    const columnCount = table.getVisibleLeafColumns().length
    if (!rowCount || !columnCount) return
    const next = {
      row: Math.max(0, Math.min(row, rowCount - 1)),
      column: Math.max(0, Math.min(column, columnCount - 1)),
    }
    setActiveCell(next)
    rowVirtualizer.scrollToIndex(next.row, { align: "auto" })
    const container = tableContainerRef.current
    if (container) {
      const visible = table.getVisibleLeafColumns()
      const widths = visible.map(col => col.id === "_select" ? 36 : col.id === "_index" ? 50 : getColWidth(col.id, columns.find(config => config.id === col.id)?.width || 180))
      const left = widths.slice(0, next.column).reduce((sum, width) => sum + width, 0)
      if (left < container.scrollLeft) container.scrollLeft = left
      else if (left + widths[next.column] > container.scrollLeft + container.clientWidth) container.scrollLeft = left + widths[next.column] - container.clientWidth
    }
    requestAnimationFrame(() => {
      tableContainerRef.current
        ?.querySelector<HTMLElement>(`[data-grid-row="${next.row}"][data-grid-column="${next.column}"]`)
        ?.focus({ preventScroll: true })
    })
  }, [rowVirtualizer, table, columns, getColWidth])

  // Quick Look (Space) for the focused grid row: the preview shows the row's
  // visible columns; ←/→ in the preview move the grid's active row with it.
  const quickLook = useQuickLook()
  const rowQuickLook = useCallback((rowIndex: number): QuickLookPayload | null => {
    const row = table.getRowModel().rows[rowIndex]?.original
    if (!row) return null
    const record = { ...(row.lead || {}), ...(row.data || {}) } as Record<string, unknown>
    const text = (value: unknown) => (value == null ? "" : typeof value === "string" ? value : JSON.stringify(value))
    const person = text(record.full_name || record.contact_person)
    const company = text(record.company || row.company)
    const website = text(record.website)
    const linkedin = text(record.linkedin_url)
    const fields: QuickLookField[] = []
    table.getVisibleLeafColumns().forEach(column => {
      if (column.id === "_select" || column.id === "_index") return
      const config = columns.find(item => item.id === column.id)
      if (!config) return
      const field = config.lead_field || config.id
      const raw = config.type === "lead_field" || config.type === "input"
        ? record[field] : row.enrichments?.[config.id]?.value
      const value = text(raw).trim()
      if (!value) return
      fields.push({ label: config.name || field, value, href: /^https?:\/\//i.test(value) ? value : undefined })
    })
    return {
      kind: person ? "Person" : "Company",
      title: person || company || `Row ${rowIndex + 1}`,
      subtitle: [text(record.title || record.contact_title), person ? company : ""].filter(Boolean).join(" · ") || undefined,
      domain: website ? website.replace(/^https?:\/\//i, "").split("/")[0] : undefined,
      fields,
      actions: [
        ...(linkedin ? [{ label: "LinkedIn", href: linkedin, external: true }] : []),
        ...(website ? [{ label: "Website", href: /^https?:/i.test(website) ? website : `https://${website}`, external: true }] : []),
      ],
    }
  }, [columns, table])

  const gridCellValue = useCallback((rowIndex: number, columnIndex: number) => {
    const row = table.getRowModel().rows[rowIndex]?.original
    const columnId = table.getVisibleLeafColumns()[columnIndex]?.id
    if (!row || !columnId || columnId === "_select") return ""
    if (columnId === "_index") return String(rowIndex + 1)
    const config = columns.find(column => column.id === columnId)
    if (!config) return ""
    if (config.type === "lead_field" || config.type === "input") {
      return (row.data || row.lead)[config.lead_field || config.id] ?? ""
    }
    return row.enrichments?.[config.id]?.value ?? ""
  }, [columns, table])

  const handleGridKeyDown = useCallback((event: React.KeyboardEvent<HTMLTableCellElement>, row: number, column: number) => {
    const target = event.target as HTMLElement
    // Interactive descendants (including portalled dialogs) own their keys.
    // Grid shortcuts must not suppress button activation or dialog Escape/Tab.
    if (target.closest("input, textarea, select, button, a, [role=dialog], [contenteditable=true]")) return

    const shortcut = event.ctrlKey || event.metaKey
    if (event.key === " " && !shortcut && !event.shiftKey) {
      event.preventDefault()
      quickLook.open({
        count: table.getRowModel().rows.length,
        index: row,
        get: rowQuickLook,
        onIndexChange: index => focusGridCell(index, column),
        elementAt: index => tableContainerRef.current
          ?.querySelector<HTMLElement>(`[data-grid-row="${index}"][data-grid-column="${column}"]`) ?? null,
      })
      return
    }
    const anchor = selectionAnchor ?? { row, column }
    const range = {
      top: Math.min(anchor.row, row), bottom: Math.max(anchor.row, row),
      left: Math.min(anchor.column, column), right: Math.max(anchor.column, column),
    }
    if (shortcut && event.key.toLowerCase() === "c") {
      event.preventDefault()
      const text = Array.from({ length: range.bottom - range.top + 1 }, (_, rowOffset) =>
        Array.from({ length: range.right - range.left + 1 }, (_, columnOffset) =>
          String(gridCellValue(range.top + rowOffset, range.left + columnOffset))
        ).join("\t")
      ).join("\n")
      navigator.clipboard.writeText(text)
        .then(() => toast.success("Copied selection", { duration: 1200 }))
        .catch(() => toast.error("Clipboard access was denied"))
      return
    }
    if (shortcut && event.key.toLowerCase() === "d") {
      event.preventDefault()
      if (range.bottom <= range.top) {
        toast.error("Select at least two rows to fill down")
        return
      }
      const visibleRows = table.getRowModel().rows
      const visibleColumns = table.getVisibleLeafColumns()
      const updates = new Map<number, Record<string, any>>()
      for (let rowIndex = range.top + 1; rowIndex <= range.bottom; rowIndex++) {
        const targetRow = visibleRows[rowIndex]?.original
        if (targetRow?.row_id == null) continue
        for (let columnIndex = range.left; columnIndex <= range.right; columnIndex++) {
          const config = columns.find(item => item.id === visibleColumns[columnIndex]?.id)
          if (!config || (config.type !== "lead_field" && config.type !== "input")) continue
          const field = config.lead_field || config.id
          updates.set(targetRow.row_id, {
            ...(updates.get(targetRow.row_id) || {}),
            [field]: gridCellValue(range.top, columnIndex),
          })
        }
      }
      if (!updates.size) {
        toast.error("Fill down requires editable input columns")
        return
      }
      bulkUpdateWorkbookRows.mutate(
        [...updates].map(([row_id, fields]) => ({ row_id, fields })),
        {
          onSuccess: data => toast.success(`Filled ${data.updated_rows} row${data.updated_rows === 1 ? "" : "s"}`),
          onError: error => toast.error(error.message),
        },
      )
      return
    }

    let nextRow = row
    let nextColumn = column
    if (event.key === "ArrowUp") nextRow--
    else if (event.key === "ArrowDown") nextRow++
    else if (event.key === "Enter") nextRow += event.shiftKey ? -1 : 1
    else if (event.key === "ArrowLeft") nextColumn--
    else if (event.key === "ArrowRight") nextColumn++
    else if (event.key === "Tab") nextColumn += event.shiftKey ? -1 : 1
    else if (event.key === "Home") nextColumn = 0
    else if (event.key === "End") nextColumn = table.getVisibleLeafColumns().length - 1
    else if (event.key === "F2") {
      event.preventDefault()
      event.currentTarget.querySelector<HTMLElement>("[data-editable-cell]")?.dispatchEvent(
        new MouseEvent("dblclick", { bubbles: true })
      )
      return
    } else return

    event.preventDefault()
    const columnCount = table.getVisibleLeafColumns().length
    if (event.key === "Tab" && nextColumn >= columnCount) {
      nextColumn = 0
      nextRow++
    } else if (event.key === "Tab" && nextColumn < 0) {
      nextColumn = columnCount - 1
      nextRow--
    }
    if (event.shiftKey && event.key !== "Tab" && event.key !== "Enter") setSelectionAnchor(current => current ?? { row, column })
    else setSelectionAnchor(null)
    focusGridCell(nextRow, nextColumn)
  }, [bulkUpdateWorkbookRows, columns, focusGridCell, gridCellValue, quickLook, rowQuickLook, selectionAnchor, table])

  const handleGridPaste = useCallback((event: React.ClipboardEvent<HTMLTableCellElement>, startRow: number, startColumn: number) => {
    const target = event.target as HTMLElement
    if (target.matches("input, textarea, [contenteditable=true]")) return
    const matrix = event.clipboardData.getData("text/plain")
      .replace(/\r\n/g, "\n")
      .replace(/\n$/, "")
      .split("\n")
      .map(line => line.split("\t"))
    if (!matrix.length || !matrix[0].length) return

    const visibleColumns = table.getVisibleLeafColumns()
    const visibleRows = table.getRowModel().rows
    const updates = new Map<number, Record<string, string>>()
    let pastedCells = 0
    matrix.forEach((values, rowOffset) => {
      const row = visibleRows[startRow + rowOffset]?.original
      if (row?.row_id == null) return
      const rowId = row.row_id
      values.forEach((value, columnOffset) => {
        const columnId = visibleColumns[startColumn + columnOffset]?.id
        const config = columns.find(column => column.id === columnId)
        if (!config || (config.type !== "lead_field" && config.type !== "input")) return
        const field = config.lead_field || config.id
        updates.set(rowId, { ...(updates.get(rowId) || {}), [field]: value })
        pastedCells++
      })
    })
    if (!updates.size) {
      toast.error("Paste into editable input columns")
      return
    }
    event.preventDefault()
    bulkUpdateWorkbookRows.mutate(
      [...updates].map(([row_id, fields]) => ({ row_id, fields })),
      {
        onSuccess: () => {
          toast.success(`Pasted ${pastedCells} cell${pastedCells === 1 ? "" : "s"}`)
          focusGridCell(startRow + matrix.length - 1, startColumn + Math.max(...matrix.map(row => row.length)) - 1)
        },
        onError: error => toast.error(error.message),
      },
    )
  }, [bulkUpdateWorkbookRows, columns, focusGridCell, table])



  // ── CSV Import ─────────────────────────────────────────────────────────

  const handleCSVImport = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    // Release the input before awaiting the optional parser chunk, so selecting
    // the same file again still works after a download or parse failure.
    e.target.value = ""

    try {
      const { default: Papa } = await import("papaparse")
      Papa.parse(file, {
        header: true,
        skipEmptyLines: true,
        complete: (results) => {
          // A single-column CSV legitimately has no detectable delimiter.
          // Structural errors, however, must not silently shift imported data.
          const parseError = results.errors.find(error => error.code !== "UndetectableDelimiter")
          if (parseError) {
            toast.error(`Invalid CSV: ${parseError.message}`)
            return
          }
          if (!results.data?.length) {
            toast.error("Empty CSV file")
            return
          }
          const fields = (results.meta.fields || []).filter(Boolean)
          if (!fields.length) {
            toast.error("CSV needs a header row")
            return
          }
          setCsvImportDraft({ fileName: file.name, rows: results.data as Record<string, any>[], fields })
        },
        error: () => toast.error("Failed to parse CSV"),
      })
    } catch {
      toast.error("Failed to load CSV importer. Reload the page and try again.")
    }
  }, [])

  // ── CSV Export ─────────────────────────────────────────────────────────

  const handleExport = useCallback(async () => {
    if (!workbook || exportingCsv) return
    setExportingCsv(true)
    try {
      const blob = await exportWorkbookCsv(workbook.id, activeViewId, deferredGlobalFilter)
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = `${workbook.name || "export"}.csv`
      a.click()
      URL.revokeObjectURL(url)
      toast.success(`Exported ${queryTotalRows} row${queryTotalRows === 1 ? "" : "s"}`)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "CSV export failed")
    } finally {
      setExportingCsv(false)
    }
  }, [activeViewId, deferredGlobalFilter, exportingCsv, queryTotalRows, workbook])

  // ── Loading / Error ────────────────────────────────────────────────────

  if (isLoading) {
    return (
      <div className="flex items-center justify-center h-full">
        <Loader2 className="size-6 animate-spin text-primary" />
      </div>
    )
  }

  if (error || !workbook) {
    const status = (error as (Error & { status?: number }) | null)?.status
    const notFound = status === 404 || status === 403 || (!error && !workbook)
    return (
      <div className="flex flex-col items-center justify-center h-full gap-4">
        <AlertCircle className="size-8 text-destructive" />
        <p className="text-sm text-muted-foreground">
          {notFound ? "Workbook not found or you don't have access" : "Couldn't load this workbook"}
        </p>
        <div className="flex items-center gap-4">
          {!notFound && (
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              <RefreshCw /> Retry
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={() => navigate("/workbooks")}>
            <ArrowLeft /> Back to workbooks
          </Button>
        </div>
      </div>
    )
  }

  const isRunning = workbook.status === "running"
  const connectorRun = connectorRunsData?.runs[0]
  const connectorActive = connectorRun?.status === "pending" || connectorRun?.status === "running" || connectorRun?.status === "retrying"
  const filterDesc = workbook.filter_criteria
    ? Object.entries(workbook.filter_criteria).filter(([, v]) => v).map(([k, v]) => `${k}: ${v}`).join(", ")
    : "All leads"

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {workbook.status === "failed" && <div role="alert" className="flex items-start gap-2 border-b border-destructive/30 bg-background px-4 py-3 text-sm shrink-0">
        <AlertCircle aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-destructive" />
        <div><p className="font-medium">Run needs attention</p>
          <p className="text-muted-foreground">Some work failed. Open Run history and inspect cell errors before retrying. Successful cells may remain; avoid re-sending completed outputs.</p>
        </div>
      </div>}
      {/* ── Selection Toolbar ─────────────────────────────────────────── */}
      <WorkbookSelectionBar count={selectedCount} visibleCount={table.getSelectedRowModel().rows.length}
        matchingCount={queryTotalRows} allMatching={allMatchingSelected}
        busy={isFetching || deleteMut.isPending || deleteMatchingMut.isPending}
        exportDisabled={exportingCsv} finalFocus={tableContainerRef}
        onSelectAll={() => { setAllMatchingSelected(true); setRowSelection({}) }}
        onClear={() => { setRowSelection({}); setAllMatchingSelected(false) }}
        onExport={allMatchingSelected ? handleExport : handleExportSelected}
        getDeleteScope={() => allMatchingSelected
          ? { kind: "matching", count: queryTotalRows, viewId: activeViewId, search: deferredGlobalFilter }
          : { kind: "rows", rowIds: selectedWorkbookRowIds(rowSelection) }}
        onDelete={handleDeleteSelected} />

      {/* ── Toolbar ─────────────────────────────────────────────────────── */}
      <div className="flex flex-wrap items-center gap-3 px-4 py-2 border-b bg-background/95 backdrop-blur-sm shrink-0">
        <button
          onClick={() => navigate("/workbooks")}
          className="p-1.5 rounded-md hover:bg-muted transition-colors"
        >
          <ArrowLeft className="size-4" />
        </button>

        <div className="flex-1 min-w-48">
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

        {/* Saved views: switcher + filter builder */}
        <WorkbookViewBar
          workbookId={id!}
          columns={columns}
          activeViewId={activeViewId}
          onSelectView={handleSelectView}
          sorting={sorting}
          hiddenColumns={hiddenColumns}
        />

        {/* Search Bar */}
        <div className="relative">
          <Search className="absolute left-2 top-1/2 -translate-y-1/2 size-3.5 text-muted-foreground pointer-events-none" />
          <input
            type="text"
            placeholder="Search rows..."
            value={globalFilter}
            onChange={e => { setGlobalFilter(e.target.value); setWorkbookPage(1); setWorkbookCursor(null); setCursorHistory([]); setAllMatchingSelected(false); setRowSelection({}) }}
            className="w-44 pl-7 pr-2 py-1.5 rounded-md border bg-background text-xs focus:outline-none focus:ring-1 focus:ring-primary/50 placeholder:text-muted-foreground/50"
          />
          {globalFilter && (
            <button
              onClick={() => { setGlobalFilter(""); setWorkbookPage(1); setWorkbookCursor(null); setCursorHistory([]); setAllMatchingSelected(false); setRowSelection({}) }}
              className="absolute right-1.5 top-1/2 -translate-y-1/2 p-0.5 rounded hover:bg-muted"
            >
              <X className="size-3 text-muted-foreground" />
            </button>
          )}
        </div>

        {/* Column Visibility Toggle */}
        {hiddenColumns.size > 0 && (
          <div className="relative">
            <button
              onClick={() => setShowColumnVisibility(!showColumnVisibility)}
              className="inline-flex items-center gap-1 px-2 py-1 rounded text-[10px] text-muted-foreground hover:bg-muted transition-colors"
            >
              <Columns3 className="size-3" />
              {hiddenColumns.size} hidden
              <ChevronDown className="size-2.5" />
            </button>
            {showColumnVisibility && (
              <div className="absolute right-0 top-7 z-50 w-52 rounded-lg border bg-card shadow-xl py-1 animate-in fade-in zoom-in-95 duration-150">
                <div className="px-3 py-1.5 text-[10px] text-muted-foreground font-medium uppercase tracking-wide">Hidden Columns</div>
                {[...hiddenColumns].map(colId => {
                  const col = columns.find(c => c.id === colId)
                  if (!col) return null
                  return (
                    <button
                      key={colId}
                      onClick={() => {
                        setHiddenColumns(prev => {
                          const next = new Set(prev)
                          next.delete(colId)
                          return next
                        })
                      }}
                      className="w-full flex items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted transition-colors"
                    >
                      <Eye className="size-3 text-muted-foreground" />
                      <span className="truncate">{col.name}</span>
                    </button>
                  )
                })}
                <div className="h-px bg-border mx-2 my-1" />
                <button
                  onClick={() => { setHiddenColumns(new Set()); setShowColumnVisibility(false) }}
                  className="w-full flex items-center gap-2 px-3 py-1.5 text-xs text-primary hover:bg-muted transition-colors"
                >
                  <Eye className="size-3" /> Show All
                </button>
              </div>
            )}
          </div>
        )}

        {/* Actions */}
        <div className="flex max-w-full flex-wrap items-center gap-1.5" role="group" aria-label="Workbook actions">
          <input
            ref={fileInputRef}
            type="file"
            accept=".csv"
            onChange={handleCSVImport}
            className="hidden"
          />
          <Button
            variant="ghost"
            size="sm"
            onClick={() => fileInputRef.current?.click()}
            title="Import a CSV or Clay table export"
          >
            <Upload />
            Import
          </Button>

          <Button
            variant="ghost"
            size="sm"
            onClick={handleExport}
            disabled={exportingCsv}
            title="Export every row matching the current search and saved view"
          >
            {exportingCsv ? <Loader2 className="animate-spin" /> : <Download />}
            {exportingCsv ? "Exporting…" : "Export"}
          </Button>

          <div className="w-px h-5 bg-border mx-1" />

          <CostChip workbookId={id!} isRunning={isRunning} viewId={activeViewId} search={deferredGlobalFilter} onClick={openSourcePanel} />

          <Button
            variant="outline"
            size="sm"
            onClick={openSourcePanel}
            disabled={sourcePanelLoading}
            aria-busy={sourcePanelLoading}
            title="Source leads, set a budget, make this workbook living"
          >
            {sourcePanelLoading ? <Loader2 className="animate-spin" /> : <Zap />}
            {sourcePanelLoading ? "Loading Source Engine…" : "Source Engine"}
          </Button>
          {SourceEnginePanel && <SourceEnginePanel
            workbookId={id!}
            open={showSourcePanel}
            onOpenChange={setShowSourcePanel}
            onChanged={() => refetch()}
          />}

          <WorkbookRunReview key={id} workbookId={id!} viewId={activeViewId}
            viewName={activeView?.name || "All rows"} search={deferredGlobalFilter}
            running={isRunning} busy={isFetching || runMut.isPending || globalFilter !== deferredGlobalFilter}
            outputColumns={columns.filter(column => column.type === "output").map(column => column.name)} />
          <WorkbookRunHistory key={`history-${id}`} workbookId={id!} />
        </div>
      </div>

      {/* ── Table ───────────────────────────────────────────────────────── */}
      <DndContext sensors={dndSensors} collisionDetection={closestCenter} onDragEnd={handleColumnDragEnd}>
        <SortableContext
          items={columns.filter(c => !hiddenColumns.has(c.id)).map(c => c.id)}
          strategy={horizontalListSortingStrategy}
        >
      <div ref={tableContainerRef} tabIndex={-1} aria-label="Workbook grid region" className="flex-1 overflow-auto pl-2">
        <table className="border-collapse text-sm" role="grid" aria-label="Workbook data grid" style={{ tableLayout: "fixed", minWidth: "100%", width: gridColumnWidths.reduce((sum, width) => sum + width, 40) }}>
          <thead className="sticky top-0 z-10 bg-muted/80 backdrop-blur-sm">
            {table.getHeaderGroups().map(headerGroup => (
              <tr key={headerGroup.id}>
                {headerGroup.headers.filter(h => h.id === "_select" || h.id === "_index").map(header => {
                  const colConfig = columns.find(c => c.id === header.id)
                  const w = header.id === "_index" ? 50 : (header.id === "_select" ? 36 : getColWidth(header.id, colConfig?.width || 180))
                  return (
                    <th key={header.id} data-col-id={header.id}
                      className="text-left font-normal border-b border-r h-8 whitespace-nowrap overflow-hidden relative"
                      style={{ width: w, maxWidth: w }}>
                      {header.isPlaceholder ? null : flexRender(header.column.columnDef.header, header.getContext())}
                    </th>
                  )
                })}
                {headerGroup.headers.filter(h => h.id !== "_select" && h.id !== "_index").map(header => {
                  const colConfig = columns.find(c => c.id === header.id)
                  const w = getColWidth(header.id, colConfig?.width || 180)
                  return (
                    <SortableColumnHeader
                      key={header.id}
                      id={header.id}
                      w={w}
                      data-col-id={header.id}
                      className={`text-left font-normal border-b border-r last:border-r-0 h-8 whitespace-nowrap overflow-hidden relative group/th ${
                        isRunning && colConfig && (colConfig.type === 'enrichment' || colConfig.type === 'waterfall' || colConfig.type === 'ai_formula') ? 'col-running' : ''
                      }`}
                    >
                      {header.isPlaceholder
                        ? null
                        : flexRender(header.column.columnDef.header, header.getContext())}
                      {/* Column progress bar for enrichment columns */}
                      {colConfig && (colConfig.type === 'enrichment' || colConfig.type === 'waterfall' || colConfig.type === 'ai_formula') && (
                        <ColumnProgressBar rows={rows} colId={header.id} />
                      )}
                      <div
                        role="separator"
                        tabIndex={0}
                        aria-label={`Resize ${colConfig?.name || header.id} column`}
                        aria-orientation="vertical"
                        aria-valuemin={80}
                        aria-valuemax={600}
                        aria-valuenow={w}
                        aria-valuetext={`${w} pixels`}
                        title="Resize: Left/Right 10px, Shift 50px, Home minimum, End maximum"
                        onKeyDown={e => handleResizeKey(header.id, w, e)}
                        onPointerDown={e => e.stopPropagation()}
                        onMouseDown={(e) => { e.stopPropagation(); handleResizeStart(header.id, e) }}
                        className="absolute right-0 top-0 bottom-0 w-1 cursor-col-resize hover:bg-primary/40 active:bg-primary/60 focus-visible:bg-primary focus-visible:outline-2 focus-visible:outline-ring focus-visible:-outline-offset-2 transition-colors z-[5]"
                      />
                    </SortableColumnHeader>
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
                    <Dialog.Root open onOpenChange={(open, details) => {
                      if (addingColumn.current) { details.cancel(); return }
                      if (!open) setShowColPicker(false)
                    }}>
                    <Dialog.Popup size="lg" className="gtm-column-add">
                      <Dialog.Title>Add column</Dialog.Title>
                      {addColumnMut.isPending && <p role="status" className="text-xs text-muted-foreground">Saving column…</p>}
                      {addColumnMut.isError && <p role="alert" className="text-xs text-destructive">{addColumnMut.error.message} Your draft has been kept.</p>}
                      <fieldset disabled={addColumnMut.isPending} className="space-y-2.5 disabled:opacity-60">

                      {/* ── Generate with AI (NL → column) ── */}
                      <div className="space-y-1">
                        <div className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Generate with AI</div>
                        <div className="flex gap-1">
                          <input
                            value={nlInstruction}
                            onChange={e => setNlInstruction(e.target.value)}
                            onKeyDown={e => { if (e.key === "Enter") handleGenerateColumn() }}
                            placeholder="Describe it: extract the domain from the website URL"
                            className="flex-1 px-2.5 py-1.5 rounded-md border bg-background text-xs focus:outline-none focus:ring-1 focus:ring-primary/50"
                          />
                          <Button
                            variant="outline"
                            size="icon-sm"
                            onClick={handleGenerateColumn}
                            disabled={nlBusy || !nlInstruction.trim()}
                            title="Generate a column config from your description"
                            aria-label="Generate column from description"
                          >
                            {nlBusy ? <Loader2 className="animate-spin" /> : <Sparkles />}
                          </Button>
                        </div>
                        {nlExplanation && (
                          <p className="px-0.5 text-xs text-muted-foreground">
                            <Sparkles className="inline size-2.5 mr-0.5 text-primary" />
                            {nlExplanation} — review the pre-filled config below, then Add Column.
                          </p>
                        )}
                      </div>

                      {/* ── Quick Presets (Clay-style) ── */}
                      <div className="space-y-1.5">
                        <div className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Quick Add — Enrichment</div>
                        <div className="grid grid-cols-2 gap-1">
                          {[
                            { label: "Find Email", icon: Mail, target: "email", providers: ["hunter_io", "apollo_io", "crosslinked", "ddg_email"], desc: "4-provider waterfall" },
                            { label: "Find Phone", icon: Smartphone, target: "phone", providers: ["apollo_io", "website_scraper"], desc: "2-provider waterfall" },
                            { label: "Find LinkedIn", icon: LinkedInIcon, target: "linkedin_url", providers: ["social_finder", "crosslinked"], desc: "Profile lookup" },
                            { label: "Verify Email", icon: MailCheck, target: "email", providers: ["mailscout"], desc: "SMTP verification" },
                            { label: "Company Info", icon: Building2, target: "description", providers: ["website_scraper", "ddg_company"], desc: "Website + DDG" },
                            { label: "Decision Makers", icon: Users, target: "decision_makers", providers: ["crosslinked", "decision_maker"], desc: "Find contacts" },
                            { label: "Hiring Signals", icon: ChartColumn, target: "hiring_signals", providers: ["jobspy_signals"], desc: "Job postings" },
                            { label: "Social Profiles", icon: Globe, target: "facebook_url", providers: ["social_finder", "facebook_pages"], desc: "FB + socials" },
                          ].map(preset => (
                            <button
                              key={preset.label}
                              onClick={async () => {
                                const colId = preset.label.toLowerCase().replace(/\s+/g, "_")
                                const newCol: any = {
                                  id: colId, name: preset.label, type: "waterfall",
                                  width: 200, waterfall: preset.providers,
                                  target_field: preset.target,
                                }
                                if (await persistNewColumn(newCol)) setShowColPicker(false)
                              }}
                              className="flex items-start gap-2 p-2 rounded-md text-left hover:bg-accent border border-transparent hover:border-border transition-colors"
                            >
                              <preset.icon aria-hidden="true" className="size-3.5 mt-0.5 shrink-0 text-muted-foreground" />
                              <div className="min-w-0">
                                <div className="text-xs font-medium truncate">{preset.label}</div>
                                <div className="text-xs text-muted-foreground">{preset.desc}</div>
                              </div>
                            </button>
                          ))}
                        </div>
                      </div>

                      {/* ── AI Presets ── */}
                      <div className="space-y-1.5">
                        <div className="text-xs font-medium text-muted-foreground uppercase tracking-wider">AI Columns</div>
                        <div className="grid grid-cols-2 gap-1">
                          {[
                            { label: "AI Research", prompt: "Research {company} at {website}. Write a 2-sentence summary of what they do, their size, and key products.", icon: Brain },
                            { label: "ICP Match", prompt: "Score how well {company} ({specialization}, {company_size}) matches an ideal customer profile for a B2B SaaS tool. Return: High/Medium/Low with one reason.", icon: Target },
                            { label: "Personalized Intro", prompt: "Write a personalized 1-sentence intro for a cold email to {contact_person} at {company}. Reference their {specialization} work.", icon: PenLine },
                            { label: "Pain Points", prompt: "Based on {company}'s industry ({specialization}) and size ({company_size}), list their top 3 likely business pain points in bullet form.", icon: Lightbulb },
                          ].map(preset => (
                            <button
                              key={preset.label}
                              onClick={async () => {
                                const colId = preset.label.toLowerCase().replace(/\s+/g, "_")
                                const newCol: any = {
                                  id: colId, name: preset.label, type: "ai_formula",
                                  width: 300, prompt: preset.prompt,
                                }
                                if (await persistNewColumn(newCol)) setShowColPicker(false)
                              }}
                              className="flex items-center gap-2 p-2 rounded-md text-left hover:bg-accent border border-transparent hover:border-border transition-colors"
                            >
                              <preset.icon aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
                              <div className="text-xs font-medium truncate">{preset.label}</div>
                            </button>
                          ))}
                        </div>
                      </div>

                      {/* ── Divider ── */}
                      <div className="flex items-center gap-2">
                        <div className="flex-1 h-px bg-border" />
                        <span className="text-xs text-muted-foreground uppercase">or build custom</span>
                        <div className="flex-1 h-px bg-border" />
                      </div>

                      {/* ── Custom Column Builder ── */}
                      <input
                        placeholder="Column name..."
                        value={newColName}
                        onChange={e => setNewColName(e.target.value)}
                        className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
                      />

                      <div className="grid grid-cols-2 gap-1">
                        {(["lead_field", "enrichment", "waterfall", "ai_formula", "research", "formula", "http", "output"] as const).map(t => {
                          const m = COL_TYPE_META[t]
                          const Icon = m.icon
                          return (
                            <button key={t} onClick={() => setNewColType(t)}
                              className={`flex items-center gap-1.5 px-2 py-1 rounded-md text-xs transition-colors ${
                                newColType === t ? "bg-primary/10 border border-primary/30" : "hover:bg-muted border border-transparent"
                              }`}
                            >
                              <Icon className={`size-3 ${m.color}`} />
                              {m.label}
                            </button>
                          )
                        })}
                      </div>

                      {newColType === "lead_field" && (
                        <NativeSelect value={newColLeadField} onChange={e => setNewColLeadField(e.target.value)}
                          className="w-full">
                          <option value="">Map to field...</option>
                          {["company","website","email","phone","contact_person","contact_title","decision_makers","city","state","address","specialization","company_size","description","linkedin_url","twitter_url","facebook_url","hiring_signals","score","score_tier","status","notes"].map(f => (
                            <option key={f} value={f}>{f.replace(/_/g, " ")}</option>
                          ))}
                        </NativeSelect>
                      )}

                      {newColType === "ai_formula" && (
                        <div className="space-y-1">
                          {aiPresets.filter(p => p.column_type === "ai_formula").length > 0 && (
                            <NativeSelect value="" onChange={e => {
                                const p = aiPresets.find(x => x.id === e.target.value)
                                if (p) { setNewColPrompt(p.prompt); if (!newColName.trim()) setNewColName(p.name) }
                              }} className="w-full">
                              <option value="">Start from a preset…</option>
                              {aiPresets.filter(p => p.column_type === "ai_formula").map(p => (
                                <option key={p.id} value={p.id}>{p.name} — {p.description}</option>
                              ))}
                            </NativeSelect>
                          )}
                          <textarea value={newColPrompt} onChange={e => setNewColPrompt(e.target.value)}
                            placeholder="Summarize what {company} does based on {website}"
                            rows={3} className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs resize-none" />
                        </div>
                      )}

                      {newColType === "research" && (
                        <div className="space-y-1">
                          {aiPresets.filter(p => p.column_type === "research").length > 0 && (
                            <NativeSelect value="" onChange={e => {
                                const p = aiPresets.find(x => x.id === e.target.value)
                                if (p) { setNewColPrompt(p.prompt); if (!newColName.trim()) setNewColName(p.name) }
                              }} className="w-full">
                              <option value="">Start from a preset…</option>
                              {aiPresets.filter(p => p.column_type === "research").map(p => (
                                <option key={p.id} value={p.id}>{p.name} — {p.description}</option>
                              ))}
                            </NativeSelect>
                          )}
                          <textarea value={newColPrompt} onChange={e => setNewColPrompt(e.target.value)}
                            placeholder="Does {company} use Kubernetes? Cite a source."
                            rows={2} className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs resize-none" />
                          <label className="flex items-center justify-between text-xs text-muted-foreground px-0.5">
                            <span>Max web steps (search/fetch)</span>
                            <input type="number" min={1} max={6} value={newColMaxSteps}
                              onChange={e => setNewColMaxSteps(Math.max(1, Math.min(6, Number(e.target.value) || 4)))}
                              className="w-14 px-1.5 py-0.5 rounded border bg-background text-xs text-right" />
                          </label>
                          <p className="px-0.5 text-xs text-muted-foreground">Agent searches the web + reads pages to answer per row. Higher steps = deeper but slower/costlier.</p>
                        </div>
                      )}

                      {newColType === "formula" && (
                        <div className="space-y-1">
                          <input value={newColFormula} onChange={e => setNewColFormula(e.target.value)}
                            placeholder={'{Email}.split("@")[1]'}
                            className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                          <p className="px-0.5 text-xs text-muted-foreground">Compute from other columns. Supports split/upper/lower/replace/default()/concat + math. Safe — no code execution.</p>
                        </div>
                      )}

                      {newColType === "http" && (
                        <div className="space-y-1">
                          <div className="flex gap-1">
                            <NativeSelect value={newColHttpMethod} onChange={e => setNewColHttpMethod(e.target.value as any)}
                            >
                              <option value="GET">GET</option>
                              <option value="POST">POST</option>
                            </NativeSelect>
                            <input value={newColHttpUrl} onChange={e => setNewColHttpUrl(e.target.value)}
                              placeholder="https://api.example.com/find?domain={website}"
                              className="flex-1 px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                          </div>
                          {newColHttpMethod === "POST" && (
                            <textarea value={newColHttpBody} onChange={e => setNewColHttpBody(e.target.value)}
                              placeholder='JSON body, e.g. {"company": "{company}"}'
                              rows={2} className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono resize-none" />
                          )}
                          <input value={newColHttpExtract} onChange={e => setNewColHttpExtract(e.target.value)}
                            placeholder="Extract: $.data.email (JSONPath; blank = raw text)"
                            className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                          <p className="px-0.5 text-xs text-muted-foreground">Calls an API per row. {'{column}'} placeholders resolve to row values. URLs are SSRF-guarded.</p>
                        </div>
                      )}

                      {newColType === "enrichment" && (
                        <NativeSelect value={newColProvider} onChange={e => setNewColProvider(e.target.value)}
                          className="w-full">
                          <option value="">Select provider...</option>
                          {availableProviders.map(p => (
                            <option key={p.name} value={p.name}>{p.name} · {p.maturity} ({p.capabilities.join(", ")})</option>
                          ))}
                        </NativeSelect>
                      )}

                      {newColType === "waterfall" && (
                        <div className="space-y-1">
                          {newColWaterfall.map((pName, i) => (
                            <div key={pName} className="flex items-center gap-1 px-2 py-0.5 rounded bg-muted/50 text-xs">
                              <span className="text-muted-foreground w-3">{i+1}.</span>
                              <span className="flex-1 truncate">{pName}</span>
                              <button onClick={() => setNewColWaterfall(prev => prev.filter((_, j) => j !== i))} className="p-0.5 hover:text-destructive"><X className="size-2.5" /></button>
                            </div>
                          ))}
                          <NativeSelect value="" onChange={e => { if (e.target.value && !newColWaterfall.includes(e.target.value)) setNewColWaterfall(prev => [...prev, e.target.value]) }}
                            className="w-full">
                            <option value="">+ Add provider...</option>
                            {availableProviders.filter(p => !newColWaterfall.includes(p.name)).map(p => (
                              <option key={p.name} value={p.name}>{p.name}</option>
                            ))}
                          </NativeSelect>
                        </div>
                      )}

                      {(newColType === "enrichment" || newColType === "waterfall") && (
                        <NativeSelect value={newColTargetField} onChange={e => setNewColTargetField(e.target.value)}
                          className="w-full">
                          <option value="">Target field (writes to Lead)...</option>
                          {["email","phone","website","contact_person","contact_title","linkedin_url","twitter_url","facebook_url","description","company_size","decision_makers","hiring_signals"].map(f => (
                            <option key={f} value={f}>{f.replace(/_/g, " ")}</option>
                          ))}
                        </NativeSelect>
                      )}

                      {newColType === "output" && (
                        <div className="space-y-1">
                          <NativeSelect value={newColDest} onChange={e => setNewColDest(e.target.value as any)}
                            className="w-full">
                            <option value="webhook">Webhook (HTTP POST)</option>
                            <option value="crm">CRM (HubSpot / Salesforce)</option>
                            <option value="sequencer">Email sequencer</option>
                            <option value="airtable">Airtable</option>
                            <option value="sheets">Google Sheets</option>
                          </NativeSelect>
                          {newColDest === "webhook" && (
                            <>
                              <input value={newColWebhookUrl} onChange={e => setNewColWebhookUrl(e.target.value)}
                                placeholder="https://hooks.example.com/{company}"
                                className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                              <textarea value={newColWebhookBody} onChange={e => setNewColWebhookBody(e.target.value)}
                                placeholder={'Optional JSON body, e.g. {"co": "{company}", "email": "{email}"}'}
                                rows={2} className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono resize-none" />
                            </>
                          )}
                          {newColDest === "crm" && (
                            <>
                              <NativeSelect value={newColCrmType} onChange={e => setNewColCrmType(e.target.value as any)}
                                className="w-full">
                                <option value="hubspot">HubSpot</option>
                                <option value="salesforce">Salesforce</option>
                              </NativeSelect>
                              <p className="px-1 text-xs text-muted-foreground">
                                Pushes the row as a {newColCrmType === "hubspot" ? "HubSpot contact" : "Salesforce lead"} (set the token in Settings).
                              </p>
                            </>
                          )}
                          {newColDest === "sequencer" && (
                            <input value={newColSequenceId} onChange={e => setNewColSequenceId(e.target.value)}
                              placeholder="Sequence ID to enroll the lead into"
                              className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                          )}
                          {newColDest === "airtable" && (
                            <>
                              <input value={newColAirtableBase} onChange={e => setNewColAirtableBase(e.target.value)}
                                placeholder="Base ID (appXXXXXXXX)"
                                className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                              <input value={newColAirtableTable} onChange={e => setNewColAirtableTable(e.target.value)}
                                placeholder="Table name or ID"
                                className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                              <p className="px-1 text-xs text-muted-foreground">Appends Company/Email/Phone/Website/City (set AIRTABLE_TOKEN in Settings).</p>
                            </>
                          )}
                          {newColDest === "sheets" && (
                            <>
                              <input value={newColSheetId} onChange={e => setNewColSheetId(e.target.value)}
                                placeholder="Spreadsheet ID"
                                className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                              <input value={newColSheetRange} onChange={e => setNewColSheetRange(e.target.value)}
                                placeholder="Sheet/tab name (e.g. Sheet1)"
                                className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono" />
                              <p className="px-1 text-xs text-muted-foreground">Appends a row (set GOOGLE_SHEETS_TOKEN in Settings).</p>
                            </>
                          )}
                        </div>
                      )}

                      {newColType !== "lead_field" && (
                        <input value={newColCondition} onChange={e => setNewColCondition(e.target.value)}
                          placeholder='Run if: {email} == "" AND {website} != ""'
                          className="w-full px-2.5 py-1 rounded-md border bg-background text-xs font-mono" />
                      )}

                      <div className="flex gap-2 justify-end pt-0.5">
                        <Button variant="ghost" size="sm" onClick={() => { setShowColPicker(false); setNewColName(""); setNewColPrompt(""); setNewColCondition(""); setNewColLeadField(""); setNlInstruction(""); setNlExplanation("") }}>Cancel</Button>
                        <Button
                          size="sm"
                          onClick={async () => {
                            if (!newColName.trim()) return
                            const colId = newColName.toLowerCase().replace(/\s+/g, "_")
                            const newCol: any = { id: colId, name: newColName.trim(), type: newColType, width: newColType === "ai_formula" ? 300 : 200 }
                            if (newColType === "lead_field" && newColLeadField) newCol.lead_field = newColLeadField
                            if (newColType === "enrichment" && newColProvider) newCol.provider = newColProvider
                            if (newColType === "waterfall" && newColWaterfall.length > 0) newCol.waterfall = newColWaterfall
                            if ((newColType === "enrichment" || newColType === "waterfall") && newColTargetField) newCol.target_field = newColTargetField
                            if (newColType === "ai_formula" && newColPrompt.trim()) newCol.prompt = newColPrompt.trim()
                            if (newColType === "research") {
                              newCol.prompt = newColPrompt.trim()
                              newCol.max_steps = newColMaxSteps
                              newCol.width = 320
                            }
                            if (newColType === "formula" && newColFormula.trim()) newCol.formula = newColFormula.trim()
                            if (newColType === "http" && newColHttpUrl.trim()) {
                              newCol.http_url = newColHttpUrl.trim()
                              newCol.http_method = newColHttpMethod
                              if (newColHttpExtract.trim()) newCol.http_extract = newColHttpExtract.trim()
                              if (newColHttpMethod === "POST" && newColHttpBody.trim()) {
                                try { newCol.http_body = JSON.parse(newColHttpBody) } catch { newCol.http_body = newColHttpBody }
                              }
                            }
                            if (newColType === "output") {
                              newCol.destination = newColDest
                              if (newColDest === "webhook") {
                                const cfg: any = { url: newColWebhookUrl.trim() }
                                if (newColWebhookBody.trim()) {
                                  try { cfg.body = JSON.parse(newColWebhookBody) } catch { cfg.body = newColWebhookBody }
                                }
                                newCol.destination_config = cfg
                              } else if (newColDest === "crm") {
                                newCol.destination_config = { type: newColCrmType }
                              } else if (newColDest === "sequencer") {
                                newCol.destination_config = { sequence_id: newColSequenceId.trim() }
                              } else if (newColDest === "airtable") {
                                newCol.destination_config = { base_id: newColAirtableBase.trim(), table: newColAirtableTable.trim() }
                              } else if (newColDest === "sheets") {
                                newCol.destination_config = { spreadsheet_id: newColSheetId.trim(), range: newColSheetRange.trim() || "Sheet1" }
                              }
                            }
                            if (newColCondition.trim()) newCol.condition = newColCondition.trim()
                            if (!await persistNewColumn(newCol)) return
                            setShowColPicker(false); setNewColName(""); setNewColPrompt(""); setNewColCondition("")
                            setNewColFormula(""); setNewColHttpUrl(""); setNewColHttpExtract(""); setNewColHttpBody("")
                            setNewColLeadField(""); setNewColType("lead_field"); setNewColProvider(""); setNewColWaterfall([]); setNewColTargetField("")
                            setNewColDest("webhook"); setNewColWebhookUrl(""); setNewColWebhookBody(""); setNewColSequenceId(""); setNewColMaxSteps(4)
                            setNewColCrmType("hubspot"); setNewColAirtableBase(""); setNewColAirtableTable(""); setNewColSheetId(""); setNewColSheetRange("Sheet1")
                            setNlInstruction(""); setNlExplanation("")
                          }}
                          disabled={!newColName.trim() || addColumnMut.isPending}
                        >Add Column</Button>
                      </div>
                      </fieldset>
                    </Dialog.Popup>
                    </Dialog.Root>
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
                  className="group"
                >
                  {columnWindow.map(segment => {
                    if (segment.kind === "spacer") return <td key={`spacer-${segment.start}`} aria-hidden="true" role="presentation" colSpan={segment.end - segment.start} style={{ width: segment.width }} className="border-b p-0" />
                    const cellIndex = segment.index
                    const cell = row.getVisibleCells()[cellIndex]
                    const colConfig = columns.find(c => c.id === cell.column.id)
                    const w = cell.column.id === "_index" ? 50 : getColWidth(cell.column.id, colConfig?.width || 180)
                    return (
                    <td
                      key={cell.id}
                      data-col-id={cell.column.id}
                      data-grid-row={virtualRow.index}
                      data-grid-column={cellIndex}
                      tabIndex={activeCell.row === virtualRow.index && activeCell.column === cellIndex ? 0 : -1}
                      onPointerDown={event => {
                        if (event.shiftKey) setSelectionAnchor(current => current ?? activeCell)
                        else setSelectionAnchor(null)
                      }}
                      onFocus={() => setActiveCell({ row: virtualRow.index, column: cellIndex })}
                      onKeyDown={event => handleGridKeyDown(event, virtualRow.index, cellIndex)}
                      onPaste={event => handleGridPaste(event, virtualRow.index, cellIndex)}
                      className={`border-b border-r last:border-r-0 h-9 p-0 overflow-hidden outline-none focus-visible:ring-2 focus-visible:ring-primary focus-visible:ring-inset ${
                        selectionAnchor && virtualRow.index >= Math.min(selectionAnchor.row, activeCell.row)
                          && virtualRow.index <= Math.max(selectionAnchor.row, activeCell.row)
                          && cellIndex >= Math.min(selectionAnchor.column, activeCell.column)
                          && cellIndex <= Math.max(selectionAnchor.column, activeCell.column)
                          ? "bg-primary/10" : activeCell.row === virtualRow.index && activeCell.column === cellIndex ? "bg-primary/[0.04]" : ""
                      }`}
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
            <Button size="sm" onClick={() => fileInputRef.current?.click()}>
              <Upload />
              Import CSV
            </Button>
          </div>
        )}
      </div>
        </SortableContext>
      </DndContext>

      {/* ── Column Context Menu ─────────────────────────────────────── */}
      {ctxMenu && (() => {
        const col = columns.find(c => c.id === ctxMenu.colId)
        if (!col) return null
        const tableCol = table.getColumn(ctxMenu.colId)
        const isEnrichable = ["enrichment", "waterfall", "ai_formula", "output", "research"].includes(col.type)
        return (
          <div
            className="fixed z-[100] min-w-[180px] rounded-lg border bg-card shadow-xl py-1 animate-in fade-in zoom-in-95 duration-150"
            style={{ left: ctxMenu.x, top: ctxMenu.y }}
            onClick={e => e.stopPropagation()}
          >
            <div className="px-3 py-1.5 text-[10px] font-medium text-muted-foreground uppercase tracking-wide">{col.name}</div>
            <button
              onClick={() => handleRenameColumn(ctxMenu.colId)}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted transition-colors"
            >
              <Pencil className="size-3.5" /> Rename
            </button>
            <button
              onClick={() => { setConfigPanelColId(ctxMenu.colId); setCtxMenu(null) }}
              className="w-full flex items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted transition-colors"
            >
              <Settings className="size-3.5" /> Configure
            </button>
            <div className="h-px bg-border mx-2 my-1" />
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
            {isEnrichable && (
              <>
                <div className="h-px bg-border mx-2 my-1" />
                <button
                  onClick={() => handleRunSingleColumn(ctxMenu.colId)}
                  className="w-full flex items-center gap-2 px-3 py-1.5 text-xs text-blue-400 hover:bg-blue-500/10 transition-colors"
                >
                  <Zap className="size-3.5" /> Run This Column
                </button>
                <button
                  onClick={() => handleForceRunColumn(ctxMenu.colId)}
                  title="Re-run every cell, bypassing success-skip gates (output columns re-push)"
                  className="w-full flex items-center gap-2 px-3 py-1.5 text-xs text-amber-400 hover:bg-amber-500/10 transition-colors"
                >
                  <RefreshCw className="size-3.5" /> Re-run Column (force)
                </button>
              </>
            )}
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

      {/* ── Inline Rename Overlay ────────────────────────────────────── */}
      {deleteColumnScope && <DeleteColumnDialog workbookId={id!} column={deleteColumnScope}
        onClose={() => setDeleteColumnScope(null)} finalFocus={tableContainerRef} />}
      {renamingColId && (() => {
        const col = columns.find(c => c.id === renamingColId)
        if (!col) return null
        return (
          <Dialog.Root open onOpenChange={(open, details) => {
            if (renameSubmitting.current) { details.cancel(); return }
            if (!open) setRenamingColId(null)
          }}>
            <Dialog.Popup size="sm" finalFocus={() => {
              const header = Array.from(tableContainerRef.current?.querySelectorAll<HTMLElement>("th[data-col-id]") || [])
                .find(element => element.dataset.colId === col.id)
              return header?.querySelector<HTMLElement>("[data-column-settings-trigger]") || tableContainerRef.current
            }}>
              <Dialog.Header><Dialog.Title>Rename column</Dialog.Title></Dialog.Header>
              <Dialog.Body>
              <label htmlFor="rename-column-name" className="text-xs font-medium">Column name</label>
              <DesignInput id="rename-column-name" disabled={renameSave.isPending}
                value={renameValue}
                onChange={e => setRenameValue(e.target.value)}
                onKeyDown={e => { if (e.key === "Enter") { e.preventDefault(); void handleRenameSubmit() } }}
              />
              {renameSave.isError && <p role="alert" className="mt-2 text-xs text-destructive">{renameSave.error.message} Your draft is kept. Retry, or cancel and refresh the workbook before reviewing another edit.</p>}
              </Dialog.Body>
              <Dialog.Footer>
                <Dialog.Close render={<DesignButton disabled={renameSave.isPending} />}>Cancel</Dialog.Close>
                <DesignButton onClick={() => void handleRenameSubmit()} disabled={!renameValue.trim() || renameSave.isPending}>{renameSave.isPending ? "Saving…" : "Rename"}</DesignButton>
              </Dialog.Footer>
            </Dialog.Popup>
          </Dialog.Root>
        )
      })()}

      {/* ── Column Config Side Panel ─────────────────────────────────── */}
      {configPanelColId && (() => {
        const col = columns.find(c => c.id === configPanelColId)
        if (!col) return null
        const meta = COL_TYPE_META[col.type] || COL_TYPE_META.lead_field
        const Icon = meta.icon

        const updateCol = (updates: Partial<typeof col>) => {
          if (settingsSubmitting.current || settingsSave.isError) return
          const changes = Object.fromEntries(Object.entries(updates).map(([key, value]) => [key, value ?? null]))
          const expected = Object.fromEntries(Object.keys(changes).map(key => [key, col[key as keyof typeof col] ?? null]))
          settingsSubmitting.current = true
          settingsSave.mutate({ columnId: col.id, changes, expected }, {
            onError: error => toast.error(error.message),
            onSettled: () => { settingsSubmitting.current = false },
          })
        }

        return (
          <Dialog.Root open onOpenChange={open => { if (!open) setConfigPanelColId(null) }}>
          <Dialog.Popup className="gtm-column-settings" finalFocus={() => {
            const header = Array.from(tableContainerRef.current?.querySelectorAll<HTMLElement>("th[data-col-id]") || [])
              .find(element => element.dataset.colId === col.id)
            return header?.querySelector<HTMLElement>("[data-column-settings-trigger]")
              || (configTrigger.current?.isConnected ? configTrigger.current : tableContainerRef.current)
          }}>
            {/* Header */}
            <div className="flex items-center justify-between px-4 py-3 border-b">
              <div className="flex items-center gap-2">
                <Icon className={`size-4 ${meta.color}`} />
                <Dialog.Title className="text-sm font-semibold truncate">{col.name} column settings</Dialog.Title>
              </div>
              <button aria-label="Close column settings" onClick={() => setConfigPanelColId(null)}
                className="p-1 rounded hover:bg-muted"><X className="size-4" /></button>
            </div>

            {/* Body */}
            <div className="flex-1 min-h-0 overflow-y-auto p-4 space-y-4">
              <ColumnWidthEditor key={`${id}:${col.id}`} workbookId={id!} columnId={col.id}
                width={getColWidth(col.id, col.width || 180)} onSaved={width => {
                  columnWidthsRef.current[col.id] = width
                  forceResizeRender(n => n + 1)
                }} />
              {/* Type badge */}
              <div className="flex items-center gap-2">
                <span className={`inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium ${meta.color} bg-muted/50`}>
                  <Icon className="size-3" /> {meta.label}
                </span>
                <span className="text-xs text-muted-foreground">ID: {col.id}</span>
              </div>

              {/* Column Name */}
              {settingsSave.isPending && <p role="status" className="text-xs text-muted-foreground">Saving column settings…</p>}
              {settingsSave.isError && <div className="space-y-2">
                <p role="alert" className="text-xs text-destructive">{settingsSave.error.message} Submitted values for column {settingsSave.variables?.columnId} are retained for retry.</p>
                <Button variant="outline" size="xs" onClick={() => {
                  if (settingsSave.variables && !settingsSubmitting.current) {
                    settingsSubmitting.current = true
                    settingsSave.mutate(settingsSave.variables, { onSettled: () => { settingsSubmitting.current = false } })
                  }
                }}>Retry settings save</Button>
                <Button variant="link" size="xs" className="block" onClick={async () => {
                  const result = await refetch()
                  if (result.isError) { toast.error("Could not reload settings. Your submitted values are still retained."); return }
                  settingsSave.reset()
                  setConfigPanelColId(null)
                }}>Discard submitted edit and reload settings</Button>
              </div>}
              <fieldset disabled={settingsSave.isPending || settingsSave.isError} className="space-y-4 disabled:opacity-60">
              <div className="space-y-1.5">
                <label htmlFor="column-settings-name" className="text-xs text-muted-foreground font-medium">Column Name</label>
                <input
                  id="column-settings-name"
                  defaultValue={col.name}
                  onBlur={e => { if (e.target.value.trim() && e.target.value !== col.name) updateCol({ name: e.target.value.trim() }) }}
                  className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
                />
              </div>

              {/* Lead Field mapping */}
              {(col.type === "lead_field" || col.type === "input") && (
                <div className="space-y-1.5">
                  <label className="text-xs text-muted-foreground font-medium">Mapped Lead Field</label>
                  <NativeSelect
                    defaultValue={col.lead_field || ""}
                    onChange={e => updateCol({ lead_field: e.target.value })}
                    className="w-full"
                  >
                    <option value="">None</option>
                    {["company", "website", "email", "phone", "contact_person", "contact_title",
                      "city", "state", "specialization", "company_size", "description",
                      "linkedin_url", "twitter_url", "facebook_url", "score", "score_tier",
                      "status", "source", "notes"].map(f => (
                      <option key={f} value={f}>{f.replace(/_/g, " ")}</option>
                    ))}
                  </NativeSelect>
                </div>
              )}

              {/* Provider (enrichment) */}
              {col.type === "enrichment" && (
                <div className="space-y-1.5">
                  <label className="text-xs text-muted-foreground font-medium">Provider</label>
                  <NativeSelect
                    defaultValue={col.provider || ""}
                    onChange={e => updateCol({ provider: e.target.value })}
                    className="w-full"
                  >
                    <option value="">Select provider...</option>
                    {availableProviders.map(p => (
                      <option key={p.name} value={p.name}>{p.name} · {p.maturity} ({p.capabilities.join(", ")})</option>
                    ))}
                  </NativeSelect>
                </div>
              )}

              {/* Waterfall chain (waterfall) */}
              {col.type === "waterfall" && (
                <div className="space-y-1.5">
                  <label className="text-xs text-muted-foreground font-medium">
                    Provider Chain <span className="text-muted-foreground/50">(first match wins)</span>
                  </label>
                  <div className="space-y-1">
                    {(col.waterfall || []).map((pName: string, i: number) => (
                      <div key={pName} className="flex items-center gap-1.5 px-2 py-1.5 rounded-md bg-muted/50 text-xs">
                        <GripVertical className="size-3 text-muted-foreground/40 shrink-0" />
                        <span className="text-xs text-muted-foreground tabular-nums w-4">{i + 1}.</span>
                        <span className="flex-1 truncate">{pName}</span>
                        <button
                          onClick={() => {
                            const chain = [...(col.waterfall || [])]
                            chain.splice(i, 1)
                            updateCol({ waterfall: chain })
                          }}
                          className="p-0.5 rounded hover:bg-destructive/10 hover:text-destructive"
                        >
                          <X className="size-3" />
                        </button>
                      </div>
                    ))}
                  </div>
                  <NativeSelect
                    value=""
                    onChange={e => {
                      if (e.target.value) {
                      updateCol({ waterfall: [...(col.waterfall || []), e.target.value] })
                    }
                    }}
                    className="w-full"
                  >
                    <option value="">+ Add provider...</option>
                    {availableProviders
                      .filter(p => !(col.waterfall || []).includes(p.name))
                      .map(p => (
                        <option key={p.name} value={p.name}>{p.name} · {p.maturity} ({p.capabilities.join(", ")})</option>
                      ))}
                  </NativeSelect>
                </div>
              )}

              {/* Target field (enrichment/waterfall) */}
              {(col.type === "enrichment" || col.type === "waterfall") && (
                <div className="space-y-1.5">
                  <label className="text-xs text-muted-foreground font-medium">Target Lead Field</label>
                  <NativeSelect
                    defaultValue={col.target_field || ""}
                    onChange={e => updateCol({ target_field: e.target.value || undefined })}
                    className="w-full"
                  >
                    <option value="">Same as column name</option>
                    {["email", "phone", "website", "contact_person", "contact_title",
                      "linkedin_url", "twitter_url", "facebook_url",
                      "description", "company_size", "industry_tags",
                      "decision_makers", "hiring_signals"].map(f => (
                      <option key={f} value={f}>{f.replace(/_/g, " ")}</option>
                    ))}
                  </NativeSelect>
                </div>
              )}

              {/* AI Prompt (ai_formula) */}
              {col.type === "ai_formula" && (
                <div className="space-y-1.5">
                  <label className="text-xs text-muted-foreground font-medium">
                    AI Prompt <span className="text-muted-foreground/50">— use {"{column}"} placeholders</span>
                  </label>
                  <textarea
                    defaultValue={col.prompt || ""}
                    onBlur={e => updateCol({ prompt: e.target.value })}
                    placeholder="Summarize what {company} does based on {website}"
                    rows={4}
                    className="w-full px-2.5 py-1.5 rounded-md border bg-background text-sm focus:outline-none focus:ring-1 focus:ring-primary/50 resize-none font-mono text-xs"
                  />
                </div>
              )}

              {/* Condition (all non-lead_field) */}
              {col.type !== "lead_field" && col.type !== "input" && (
                <div className="space-y-1.5">
                  <label className="text-xs text-muted-foreground font-medium">Only Run If</label>
                  <input
                    defaultValue={col.condition || ""}
                    onBlur={e => updateCol({ condition: e.target.value || undefined })}
                    placeholder='{email} == "" AND {website} != ""'
                    className="w-full px-2.5 py-1.5 rounded-md border bg-background text-xs font-mono focus:outline-none focus:ring-1 focus:ring-primary/50"
                  />
                  <p className="text-xs text-muted-foreground/50">
                    Leave empty to always run. Supports ==, !=, &gt;, &lt;, AND, OR.
                  </p>
                </div>
              )}

              {/* Reactive dependency policy */}
              {col.type !== "lead_field" && col.type !== "input" && (
                <div className="space-y-1.5 rounded-md border p-2.5">
                  <label className="flex items-center gap-2 text-xs font-medium cursor-pointer">
                    <input
                      type="checkbox"
                      checked={col.reactive ?? col.type !== "output"}
                      onChange={e => updateCol({ reactive: e.target.checked })}
                      className="size-3.5 rounded border-muted-foreground/40 accent-primary"
                    />
                    Recompute after upstream edits
                  </label>
                  <p className="text-xs leading-relaxed text-muted-foreground/60">
                    Queues this column and its dependents when referenced input values change. Output columns default off to prevent unintended external actions.
                  </p>
                </div>
              )}

              </fieldset>
            </div>

            {/* Footer */}
            <div className="px-4 py-3 border-t flex items-center justify-between">
              <Button
                variant="destructive"
                size="sm"
                disabled={settingsSave.isPending || settingsSave.isError}
                onClick={() => handleDeleteColumn(configPanelColId)}
              >
                <Trash2 /> Delete
              </Button>
              {["enrichment", "waterfall", "ai_formula"].includes(col.type) && (
                <Button
                  size="sm"
                  disabled={settingsSave.isPending || settingsSave.isError}
                  onClick={() => { handleRunSingleColumn(configPanelColId); setConfigPanelColId(null) }}
                >
                  <Zap /> Run Column
                </Button>
              )}
            </div>
          </Dialog.Popup>
          </Dialog.Root>
        )
      })()}

      {csvImportDraft && (
        <CsvImportDialog
          draft={csvImportDraft}
          columns={columns}
          importing={importLeadsMut.isPending}
          onClose={() => { if (!importLeadsMut.isPending) setCsvImportDraft(null) }}
          onImport={async options => {
            try {
              const result = await importLeadsMut.mutateAsync(options)
              const duplicateNote = result.skipped_duplicates
                ? ` · ${result.skipped_duplicates} duplicate${result.skipped_duplicates === 1 ? "" : "s"} skipped`
                : ""
              const columnNote = result.columns_added.length
                ? ` · ${result.columns_added.length} column${result.columns_added.length === 1 ? "" : "s"} added`
                : ""
              toast.success(`Imported ${result.added} row${result.added === 1 ? "" : "s"}${duplicateNote}${columnNote}`)
              setCsvImportDraft(null)
            } catch (error) {
              toast.error(error instanceof Error ? error.message : "CSV import failed")
            }
          }}
        />
      )}

      {/* ── Activity Drawer (slides up from status bar) ─────────────── */}
      <ActivityDrawer
        rows={rows}
        columns={columns}
        isRunning={workbook?.status === "running"}
        connected={connected}
        isOpen={activityOpen}
        onToggle={() => setActivityOpen(!activityOpen)}
      />

      {/* ── Status Bar ───────────────────────────────────────────────── */}
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-1 border-t text-[11px] text-muted-foreground bg-muted/30 shrink-0">
        <div className="flex flex-wrap items-center gap-3 min-w-0">
          <span className="tabular-nums">
            {queryTotalRows ? `${(displayedWorkbookPage - 1) * workbookPageSize + 1}–${Math.min(displayedWorkbookPage * workbookPageSize, queryTotalRows)}` : "0"} of {queryTotalRows} rows
            {queryTotalRows !== (data?.total_rows ?? 0) && (
              <span className="text-primary/70"> · {data?.total_rows ?? 0} total{activeView ? ` · view “${activeView.name}”` : ""}</span>
            )}
          </span>
          {totalPages > 1 && (
            <span className="inline-flex items-center overflow-hidden rounded border bg-background">
              <button
                type="button"
                aria-label="Previous workbook page"
                disabled={workbookPage <= 1 || isFetching}
                onClick={() => changeWorkbookPage(workbookPage - 1)}
                className="px-2 py-0.5 hover:bg-muted disabled:opacity-40"
              >‹</button>
              <span className="border-x px-2 py-0.5 tabular-nums">{isFetching ? "Loading…" : `Page ${workbookPage} / ${totalPages}`}</span>
              <button
                type="button"
                aria-label="Next workbook page"
                disabled={workbookPage >= totalPages || isFetching}
                onClick={() => changeWorkbookPage(workbookPage + 1)}
                className="px-2 py-0.5 hover:bg-muted disabled:opacity-40"
              >›</button>
            </span>
          )}
          <span className="text-border">│</span>
          <span>{columns.length} columns</span>
          {columns.filter(c => c.type === "waterfall" || c.type === "enrichment").length > 0 && (
            <>
              <span className="text-border">│</span>
              <span>{columns.filter(c => c.type === "waterfall" || c.type === "enrichment").length} enrichment</span>
            </>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-3 min-w-0 max-w-full">
          {connected && (
            <span className="inline-flex items-center gap-1 text-emerald-400">
              <span className="size-1.5 rounded-full bg-emerald-400" />
              Live
            </span>
          )}
          {connectorActive && connectorRun && (() => {
            const pct = connectorRun.requested_count > 0
              ? Math.min(100, Math.round((connectorRun.fetched_count / connectorRun.requested_count) * 100))
              : 0
            return (
              <span className="inline-flex items-center gap-2 text-blue-400" title={`Page ${connectorRun.pages_fetched} · ${connectorRun.connector}`}>
                <Loader2 className="size-3 animate-spin" />
                <span className="capitalize">{connectorRun.connector}</span>
                <span className="tabular-nums">{connectorRun.fetched_count}/{connectorRun.requested_count} rows</span>
                <span className="inline-flex items-center gap-1">
                  <span className="w-16 h-1.5 rounded-full bg-muted overflow-hidden">
                    <span className="block h-full rounded-full bg-blue-400 transition-all duration-300" style={{ width: `${pct}%` }} />
                  </span>
                  <span className="tabular-nums">{pct}%</span>
                </span>
              </span>
            )
          })()}
          {workbook?.status === "running" && !connectorActive && (() => {
            const { total: totalCells, complete, errors, skipped, running } = loadedCellProgress(rows, columns)
            const pct = totalCells > 0 ? Math.round(((complete + errors + skipped) / totalCells) * 100) : 0
            return (
              <span className="inline-flex flex-wrap items-center gap-2 text-blue-400 max-w-full">
                <Loader2 className="size-3 animate-spin" />
                <span className="tabular-nums">Loaded cells: {complete}/{totalCells} complete</span>
                {errors > 0 && <span className="text-red-400 tabular-nums">{errors} err</span>}
                {skipped > 0 && <span className="text-muted-foreground tabular-nums">{skipped} skipped</span>}
                {running > 0 && <span className="text-amber-400 tabular-nums">{running} active</span>}
                <span className="inline-flex items-center gap-1">
                  <span className="w-16 h-1.5 rounded-full bg-muted overflow-hidden">
                    <span className="h-full rounded-full bg-blue-400 transition-all duration-300" style={{ width: `${pct}%` }} />
                  </span>
                  <span className="tabular-nums">{pct}% processed</span>
                </span>
              </span>
            )
          })()}
          {/* Activity drawer toggle — inline in status bar */}
          {columns.some(c => isExecutableColumn(c.type)) && (
            <button
              className="activity-drawer-trigger"
              onClick={() => setActivityOpen(!activityOpen)}
              title={activityOpen ? "Collapse activity panel" : "Expand activity panel"}
            >
              {activityOpen ? <ChevronDown className="size-3" /> : <ChevronUp className="size-3" />}
              Activity
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
