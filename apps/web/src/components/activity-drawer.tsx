/**
 * Activity Drawer — Collapsible bottom sheet for enrichment activity logs.
 *
 * Slides up from the status bar showing real-time enrichment progress,
 * recent events, errors, and per-column stats in a professional layout.
 *
 * The drawer renders ONLY the panel. The trigger is rendered by the parent
 * (workbook-editor status bar) to keep it inline with the status text.
 */

import { useState, useEffect, useRef, useMemo, useCallback } from "react"
import {
  ChevronUp, ChevronDown, X, Loader2, Check, AlertCircle,
  Clock, Activity, Zap, Filter, Pause, RotateCcw, Trash2,
} from "lucide-react"
import type { WorkbookLeadRow, ColumnConfig, EnrichmentOverlay } from "@/lib/workbook-api"

// ── Types ─────────────────────────────────────────────────────────────────

interface ActivityEvent {
  id: string
  ts: number
  type: "enrich_start" | "enrich_complete" | "enrich_error" | "enrich_skip" | "info"
  column: string
  company: string
  provider?: string
  value?: string
  error?: string
  rowId?: number
}

interface ColumnStat {
  id: string
  name: string
  provider: string
  complete: number
  running: number
  errors: number
  pending: number
  total: number
}

export interface ActivityDrawerProps {
  rows: WorkbookLeadRow[]
  columns: ColumnConfig[]
  isRunning: boolean
  connected: boolean
  isOpen: boolean
  onToggle: () => void
}

// ── Tabs ──────────────────────────────────────────────────────────────────

type TabId = "live" | "columns" | "errors"

const TABS: { id: TabId; label: string; icon: React.ReactNode }[] = [
  { id: "live", label: "Live Feed", icon: <Activity className="size-3" /> },
  { id: "columns", label: "Columns", icon: <Zap className="size-3" /> },
  { id: "errors", label: "Errors", icon: <AlertCircle className="size-3" /> },
]

// ── Hook to compute stats (shared between drawer and status bar trigger) ──

export function useActivityStats(rows: WorkbookLeadRow[], columns: ColumnConfig[]) {
  const enrichCols = useMemo(
    () => columns.filter(c => c.type === "waterfall" || c.type === "enrichment" || c.type === "ai_formula"),
    [columns]
  )

  const stats = useMemo(() => {
    let complete = 0, errors = 0, running = 0, pending = 0
    const colStats: ColumnStat[] = []

    for (const col of enrichCols) {
      let cc = 0, ce = 0, cr = 0, cp = 0
      for (const row of rows) {
        const s = row.enrichments?.[col.id]?.status
        if (s === "complete") { cc++; complete++ }
        else if (s === "error" || s === "skipped") { ce++; errors++ }
        else if (s === "running") { cr++; running++ }
        else { cp++; pending++ }
      }
      colStats.push({
        id: col.id,
        name: col.name,
        provider: col.provider || col.waterfall?.join(" → ") || "AI",
        complete: cc,
        running: cr,
        errors: ce,
        pending: cp,
        total: rows.length,
      })
    }

    const totalCells = rows.length * enrichCols.length
    const pct = totalCells > 0 ? Math.round(((complete + errors) / totalCells) * 100) : 0

    return { complete, errors, running, pending, totalCells, pct, colStats, enrichCols }
  }, [rows, enrichCols])

  return stats
}

// ── Main Panel Component (no trigger — parent renders trigger) ─────────────

export function ActivityDrawer({ rows, columns, isRunning, connected, isOpen, onToggle }: ActivityDrawerProps) {
  const [height, setHeight] = useState(260)
  const [activeTab, setActiveTab] = useState<TabId>("live")
  const [events, setEvents] = useState<ActivityEvent[]>([])
  const [filter, setFilter] = useState("")
  const scrollRef = useRef<HTMLDivElement>(null)
  const dragRef = useRef<{ startY: number; startH: number } | null>(null)
  const activityStats = useActivityStats(rows, columns)

  const enrichCols = useMemo(
    () => columns.filter(c => c.type === "waterfall" || c.type === "enrichment" || c.type === "ai_formula"),
    [columns]
  )

  // ── Generate activity events from enrichment overlay changes ─────────

  const prevEnrichmentsRef = useRef<Record<string, string>>({})

  useEffect(() => {
    if (!isRunning) return
    const newEvents: ActivityEvent[] = []
    const current: Record<string, string> = {}

    for (const row of rows) {
      for (const col of enrichCols) {
        const overlay = row.enrichments?.[col.id]
        if (!overlay) continue
        const key = `${row.lead_id}-${col.id}`
        current[key] = overlay.status

        const prev = prevEnrichmentsRef.current[key]
        if (prev === overlay.status) continue

        if (overlay.status === "complete" && prev !== "complete") {
          newEvents.push({
            id: `${key}-${Date.now()}`,
            ts: Date.now(),
            type: "enrich_complete",
            column: col.name,
            company: row.company || `Row ${row.lead_id}`,
            provider: overlay.provider || undefined,
            value: typeof overlay.value === "string" ? overlay.value.slice(0, 80) : JSON.stringify(overlay.value)?.slice(0, 80),
            rowId: row.lead_id,
          })
        } else if (overlay.status === "error") {
          newEvents.push({
            id: `${key}-${Date.now()}`,
            ts: Date.now(),
            type: "enrich_error",
            column: col.name,
            company: row.company || `Row ${row.lead_id}`,
            provider: overlay.provider || undefined,
            error: overlay.error || "Unknown error",
            rowId: row.lead_id,
          })
        } else if (overlay.status === "running" && prev !== "running") {
          newEvents.push({
            id: `${key}-${Date.now()}`,
            ts: Date.now(),
            type: "enrich_start",
            column: col.name,
            company: row.company || `Row ${row.lead_id}`,
            provider: overlay.provider || undefined,
            rowId: row.lead_id,
          })
        }
      }
    }

    prevEnrichmentsRef.current = current
    if (newEvents.length > 0) {
      setEvents(prev => [...prev, ...newEvents].slice(-500))
    }
  }, [rows, enrichCols, isRunning])

  // Auto-scroll
  useEffect(() => {
    if (scrollRef.current && activeTab === "live") {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [events, activeTab])

  // ── Resize Handle ────────────────────────────────────────────────────

  const onDragStart = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    dragRef.current = { startY: e.clientY, startH: height }

    const onMove = (ev: MouseEvent) => {
      if (!dragRef.current) return
      const diff = dragRef.current.startY - ev.clientY
      setHeight(Math.max(160, Math.min(400, dragRef.current.startH + diff)))
    }
    const onUp = () => {
      dragRef.current = null
      window.removeEventListener("mousemove", onMove)
      window.removeEventListener("mouseup", onUp)
    }
    window.addEventListener("mousemove", onMove)
    window.addEventListener("mouseup", onUp)
  }, [height])

  // ── Filter events ────────────────────────────────────────────────────

  const filteredEvents = useMemo(() => {
    if (!filter) return events
    const q = filter.toLowerCase()
    return events.filter(e =>
      e.company.toLowerCase().includes(q) ||
      e.column.toLowerCase().includes(q) ||
      e.provider?.toLowerCase().includes(q)
    )
  }, [events, filter])

  const errorEvents = useMemo(
    () => events.filter(e => e.type === "enrich_error"),
    [events]
  )

  if (!isOpen) return null

  return (
    <div
      className="activity-drawer"
      style={{ height }}
    >
      {/* Resize handle */}
      <div className="activity-drawer-resize" onMouseDown={onDragStart}>
        <div className="activity-drawer-resize-bar" />
      </div>

      {/* Tabs */}
      <div className="activity-drawer-tabs">
        <div className="activity-drawer-tab-list">
          {TABS.map(tab => (
            <button
              key={tab.id}
              className={`activity-drawer-tab ${activeTab === tab.id ? "active" : ""}`}
              onClick={() => setActiveTab(tab.id)}
            >
              {tab.icon}
              {tab.label}
              {tab.id === "errors" && errorEvents.length > 0 && (
                <span className="activity-drawer-badge-error">{errorEvents.length}</span>
              )}
            </button>
          ))}
        </div>

        <div className="activity-drawer-tab-actions">
          {activeTab === "live" && (
            <div className="activity-drawer-filter">
              <Filter className="size-3 text-muted-foreground" />
              <input
                type="text"
                value={filter}
                onChange={e => setFilter(e.target.value)}
                placeholder="Filter events..."
                className="activity-drawer-filter-input"
              />
            </div>
          )}
          <button
            className="activity-drawer-clear"
            onClick={() => setEvents([])}
            title="Clear events"
          >
            <Trash2 className="size-3" />
          </button>
          <button
            className="activity-drawer-close"
            onClick={onToggle}
            title="Close drawer"
          >
            <ChevronDown className="size-3.5" />
          </button>
        </div>
      </div>

      {/* Tab Content */}
      <div className="activity-drawer-content" ref={scrollRef}>
        {activeTab === "live" && (
          <LiveTab events={filteredEvents} />
        )}
        {activeTab === "columns" && (
          <ColumnsTab colStats={activityStats.colStats} />
        )}
        {activeTab === "errors" && (
          <ErrorsTab errors={errorEvents} />
        )}
      </div>
    </div>
  )
}


// ── Live Feed Tab ────────────────────────────────────────────────────────

function LiveTab({ events }: { events: ActivityEvent[] }) {
  if (events.length === 0) {
    return (
      <div className="activity-drawer-empty">
        <Activity className="size-8 text-muted-foreground/20" />
        <p>No enrichment activity yet</p>
        <p className="text-[10px] text-muted-foreground/30">Run a workbook to see live enrichment logs</p>
      </div>
    )
  }

  return (
    <div className="activity-drawer-feed">
      {events.map(ev => (
        <div key={ev.id} className={`activity-event ${ev.type}`}>
          <span className="activity-event-time">
            {new Date(ev.ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}
          </span>
          <span className="activity-event-icon">
            {ev.type === "enrich_complete" && <Check className="size-3 text-emerald-400" />}
            {ev.type === "enrich_error" && <AlertCircle className="size-3 text-red-400" />}
            {ev.type === "enrich_start" && <Loader2 className="size-3 text-blue-400 animate-spin" />}
            {ev.type === "enrich_skip" && <Clock className="size-3 text-zinc-500" />}
          </span>
          <span className="activity-event-company">{ev.company}</span>
          <span className="activity-event-column">{ev.column}</span>
          {ev.provider && (
            <span className="activity-event-provider">{ev.provider}</span>
          )}
          {ev.type === "enrich_complete" && ev.value && (
            <span className="activity-event-value">{ev.value}</span>
          )}
          {ev.type === "enrich_error" && ev.error && (
            <span className="activity-event-error">{ev.error}</span>
          )}
        </div>
      ))}
    </div>
  )
}


// ── Columns Progress Tab ─────────────────────────────────────────────────

function ColumnsTab({ colStats }: { colStats: ColumnStat[] }) {
  if (colStats.length === 0) {
    return (
      <div className="activity-drawer-empty">
        <Zap className="size-8 text-muted-foreground/20" />
        <p>No enrichment columns configured</p>
      </div>
    )
  }

  return (
    <div className="activity-drawer-columns">
      {colStats.map(col => {
        const pct = col.total > 0 ? Math.round((col.complete / col.total) * 100) : 0
        return (
          <div key={col.id} className="activity-col-card">
            <div className="activity-col-header">
              <div className="activity-col-name">
                <Zap className="size-3 text-blue-400" />
                <span>{col.name}</span>
              </div>
              <span className="activity-col-provider">{col.provider}</span>
            </div>
            <div className="activity-col-bar">
              <div className="activity-col-bar-fill" style={{ width: `${pct}%` }} />
              {col.errors > 0 && (
                <div
                  className="activity-col-bar-error"
                  style={{ width: `${Math.round((col.errors / col.total) * 100)}%` }}
                />
              )}
            </div>
            <div className="activity-col-stats">
              <span className="text-emerald-400">
                <Check className="size-2.5 inline" /> {col.complete}
              </span>
              {col.running > 0 && (
                <span className="text-amber-400">
                  <Loader2 className="size-2.5 inline animate-spin" /> {col.running}
                </span>
              )}
              {col.errors > 0 && (
                <span className="text-red-400">
                  <AlertCircle className="size-2.5 inline" /> {col.errors}
                </span>
              )}
              <span className="text-muted-foreground">
                <Clock className="size-2.5 inline" /> {col.pending}
              </span>
              <span className="text-muted-foreground/50 ml-auto tabular-nums">{pct}%</span>
            </div>
          </div>
        )
      })}
    </div>
  )
}


// ── Errors Tab ───────────────────────────────────────────────────────────

function ErrorsTab({ errors }: { errors: ActivityEvent[] }) {
  if (errors.length === 0) {
    return (
      <div className="activity-drawer-empty">
        <Check className="size-8 text-emerald-400/20" />
        <p>No errors</p>
        <p className="text-[10px] text-muted-foreground/30">All enrichments completed successfully</p>
      </div>
    )
  }

  return (
    <div className="activity-drawer-feed">
      {errors.map(ev => (
        <div key={ev.id} className="activity-event enrich_error">
          <span className="activity-event-time">
            {new Date(ev.ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}
          </span>
          <AlertCircle className="size-3 text-red-400 shrink-0" />
          <span className="activity-event-company">{ev.company}</span>
          <span className="activity-event-column">{ev.column}</span>
          {ev.provider && <span className="activity-event-provider">{ev.provider}</span>}
          <span className="activity-event-error">{ev.error}</span>
        </div>
      ))}
    </div>
  )
}
