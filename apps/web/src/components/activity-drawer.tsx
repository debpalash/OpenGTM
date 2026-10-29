import { useState, useEffect, useRef, useMemo } from "react"
import {
  ChevronDown,
  Loader2,
  Check,
  AlertCircle,
  Clock,
  Activity,
  Zap,
  Trash2
} from "lucide-react"
import {
  Button,
  IconButton,
  NativeSelect,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent
} from "@/design-system/primitives"
import type { WorkbookLeadRow, ColumnConfig } from "@/lib/workbook-api"
import {
  workbookActivityChanges,
  workbookActivityErrors,
  workbookActivitySummary,
  type ActivityEvent,
  type ActivitySnapshot
} from "@/lib/workbook-activity"
import "./workbooks/activity-panel.css"

type ColumnStat = ReturnType<typeof workbookActivitySummary>[number]
interface ActivityDrawerProps {
  rows: WorkbookLeadRow[]
  columns: ColumnConfig[]
  connected: boolean
  isOpen: boolean
  onToggle: () => void
}
type TabId = "live" | "columns" | "errors"

const panelLimit = () => {
  const max = Math.max(
    160,
    Math.min(600, Math.floor(window.innerHeight * 0.55))
  )
  return {
    maxHeight: max,
    minHeight: Math.min(max, window.innerWidth < 600 ? 280 : 160)
  }
}
const storedHeight = () => {
  try {
    const saved = Number(localStorage.getItem("opengtm-activity-height"))
    if (Number.isInteger(saved) && saved >= 160 && saved <= 600) return saved
  } catch {
    /* Keep a usable in-memory layout when storage is unavailable. */
  }
  return 260
}

export function ActivityDrawer({
  rows,
  columns,
  connected,
  isOpen,
  onToggle
}: ActivityDrawerProps) {
  const [requestedHeight, setRequestedHeight] = useState(storedHeight)
  const [{ maxHeight, minHeight }, setLimits] = useState(panelLimit)
  const height = Math.max(minHeight, Math.min(requestedHeight, maxHeight))
  const [activeTab, setActiveTab] = useState<TabId>("live")
  const [events, setEvents] = useState<ActivityEvent[]>([])
  const [filter, setFilter] = useState("")
  const scrollRef = useRef<HTMLDivElement>(null)
  const dragRef = useRef<{ startY: number; startH: number } | null>(null)
  const previous = useRef<ActivitySnapshot>({})
  const colStats = useMemo(
    () => workbookActivitySummary(rows, columns),
    [rows, columns]
  )
  const errors = useMemo(
    () => workbookActivityErrors(rows, columns),
    [rows, columns]
  )

  const saveHeight = (value: number) => {
    const bounded = Math.max(minHeight, Math.min(maxHeight, Math.round(value)))
    setRequestedHeight(bounded)
    try {
      localStorage.setItem("opengtm-activity-height", String(bounded))
    } catch {
      /* In-memory choice remains available. */
    }
  }
  useEffect(() => {
    const resize = () => setLimits(panelLimit())
    window.addEventListener("resize", resize)
    return () => window.removeEventListener("resize", resize)
  }, [])
  useEffect(() => {
    const observed = workbookActivityChanges(
      rows,
      columns,
      previous.current,
      Date.now()
    )
    previous.current = observed.snapshot
    if (observed.events.length)
      setEvents((current) => [...current, ...observed.events].slice(-500))
  }, [rows, columns])
  useEffect(() => {
    if (scrollRef.current && activeTab === "live")
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
  }, [events, activeTab])

  const filteredEvents = useMemo(() => {
    const query = filter.trim().toLowerCase()
    return events.filter(
      (event) =>
        !query ||
        [event.company, event.column, event.provider].some((value) =>
          value?.toLowerCase().includes(query)
        )
    )
  }, [events, filter])
  if (!isOpen) return null
  const heights = [...new Set([minHeight, 260, 400, 600, maxHeight, height])]
    .filter((value) => value >= minHeight && value <= maxHeight)
    .sort((a, b) => a - b)

  return (
    <Tabs
      id="workbook-activity-panel"
      role="region"
      aria-label="Workbook activity"
      className="activity-drawer workbook-activity"
      value={activeTab}
      onValueChange={(value) => setActiveTab(value as TabId)}
      style={{ height }}
      onKeyDown={(event) => {
        if (
          event.key === "Escape" &&
          !event.defaultPrevented &&
          !(event.target instanceof HTMLSelectElement)
        ) {
          event.stopPropagation()
          onToggle()
        }
      }}
    >
      <div
        className="activity-drawer-resize"
        role="separator"
        tabIndex={0}
        aria-label="Resize activity panel"
        aria-orientation="horizontal"
        aria-valuemin={minHeight}
        aria-valuemax={maxHeight}
        aria-valuenow={height}
        aria-valuetext={`${height} pixels`}
        onKeyDown={(event) => {
          const step = event.shiftKey ? 50 : 10
          if (["ArrowUp", "ArrowDown", "Home", "End"].includes(event.key)) {
            event.preventDefault()
            event.stopPropagation()
            saveHeight(
              event.key === "Home"
                ? minHeight
                : event.key === "End"
                  ? maxHeight
                  : height + (event.key === "ArrowUp" ? step : -step)
            )
          }
        }}
        onPointerDown={(event) => {
          if (event.button !== 0) return
          event.preventDefault()
          dragRef.current = { startY: event.clientY, startH: height }
          event.currentTarget.setPointerCapture(event.pointerId)
        }}
        onPointerMove={(event) => {
          if (dragRef.current)
            setRequestedHeight(
              Math.max(
                minHeight,
                Math.min(
                  maxHeight,
                  Math.round(
                    dragRef.current.startH +
                      dragRef.current.startY -
                      event.clientY
                  )
                )
              )
            )
        }}
        onPointerUp={(event) => {
          if (!dragRef.current) return
          dragRef.current = null
          saveHeight(height)
          event.currentTarget.releasePointerCapture(event.pointerId)
        }}
        onPointerCancel={() => {
          dragRef.current = null
        }}
        onLostPointerCapture={() => {
          dragRef.current = null
        }}
      >
        <div className="activity-drawer-resize-bar" />
      </div>
      <div className="activity-drawer-tabs">
        <TabsList
          aria-label="Activity views"
          className="activity-drawer-tab-list"
        >
          <TabsTrigger value="live">Changes</TabsTrigger>
          <TabsTrigger value="columns">Columns</TabsTrigger>
          <TabsTrigger value="errors">
            Errors{errors.length > 0 ? ` (${errors.length})` : ""}
          </TabsTrigger>
        </TabsList>
        <div className="activity-drawer-tab-actions">
          <label className="sr-only" htmlFor="activity-panel-height">
            Activity panel height
          </label>
          <NativeSelect
            id="activity-panel-height"
            value={height}
            onChange={(event) => saveHeight(Number(event.target.value))}
          >
            {heights.map((value) => (
              <option key={value} value={value}>
                {value}px
              </option>
            ))}
          </NativeSelect>
          <IconButton
            variant="ghost"
            label="Close activity panel"
            onClick={onToggle}
          >
            <ChevronDown aria-hidden="true" className="size-4" />
          </IconButton>
        </div>
      </div>
      <div className="activity-panel-context">
        <span>
          {rows.length.toLocaleString()} loaded{" "}
          {rows.length === 1 ? "row" : "rows"} ·{" "}
          {connected ? "Updates connected" : "Updates disconnected"}
        </span>
        {activeTab === "live" && (
          <div className="activity-panel-feed-actions">
            <label className="sr-only" htmlFor="activity-event-filter">
              Filter activity events
            </label>
            <input
              id="activity-event-filter"
              type="search"
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
              placeholder="Filter events…"
            />
            <IconButton
              variant="ghost"
              label="Clear observed activity events"
              disabled={events.length === 0}
              onClick={() => setEvents([])}
            >
              <Trash2 aria-hidden="true" className="size-4" />
            </IconButton>
          </div>
        )}
      </div>
      <div className="activity-drawer-content" ref={scrollRef}>
        <TabsContent value="live">
          <p className="activity-panel-note">
            Changes observed on loaded rows while this workbook is open. Reload
            clears this feed.
          </p>
          <LiveTab events={filteredEvents} filtered={!!filter.trim()} />
        </TabsContent>
        <TabsContent value="columns">
          <ColumnsTab colStats={colStats} />
        </TabsContent>
        <TabsContent value="errors">
          <ErrorsTab errors={errors} />
        </TabsContent>
      </div>
    </Tabs>
  )
}

// ── Live Feed Tab ────────────────────────────────────────────────────────

function LiveTab({
  events,
  filtered
}: {
  events: ActivityEvent[]
  filtered: boolean
}) {
  if (events.length === 0) {
    return (
      <div className="activity-drawer-empty">
        <Activity className="size-8 text-muted-foreground/20" />
        <p>
          {filtered
            ? "No activity matches this filter"
            : "No changes observed yet"}
        </p>
        <p className="text-xs text-muted-foreground">
          {filtered
            ? "Change or clear the filter to see other observed events."
            : "Changes will appear here as loaded cells update."}
        </p>
      </div>
    )
  }

  return (
    <div className="activity-drawer-feed">
      {events.map((ev) => (
        <div key={ev.id} className={`activity-event ${ev.type}`}>
          <span className="activity-event-time">
            {new Date(ev.ts).toLocaleTimeString([], {
              hour: "2-digit",
              minute: "2-digit",
              second: "2-digit"
            })}
          </span>
          <span className="activity-event-icon">
            {ev.type === "enrich_complete" && (
              <Check className="size-3 text-[var(--gtm-success)]" />
            )}
            {ev.type === "enrich_error" && (
              <AlertCircle className="size-3 text-[var(--gtm-danger)]" />
            )}
            {ev.type === "enrich_start" && (
              <Loader2 className="size-3 text-[var(--gtm-link)] animate-spin" />
            )}
            {ev.type === "enrich_skip" && (
              <Clock className="size-3 text-muted-foreground" />
            )}
          </span>
          <span className="activity-event-company">{ev.company}</span>
          <span className="activity-event-column">{ev.column}</span>
          {ev.provider && (
            <span className="activity-event-provider">{ev.provider}</span>
          )}
          {ev.type === "enrich_complete" && ev.value && (
            <span className="activity-event-value">{ev.value}</span>
          )}
          <span className="activity-event-state">
            {ev.type === "enrich_complete"
              ? "Complete"
              : ev.type === "enrich_error"
                ? "Failed"
                : ev.type === "enrich_skip"
                  ? "Skipped"
                  : "Running"}
          </span>
          {(ev.type === "enrich_error" || ev.type === "enrich_skip") &&
            ev.error && (
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
      {colStats.map((col) => {
        const pct =
          col.total > 0 ? Math.round((col.complete / col.total) * 100) : 0
        return (
          <div
            key={col.id}
            role="group"
            aria-label={`${col.name} progress`}
            className="activity-col-card"
          >
            <div className="activity-col-header">
              <div className="activity-col-name">
                <Zap className="size-3 text-[var(--gtm-link)]" />
                <span>{col.name}</span>
              </div>
              <span className="activity-col-provider">{col.provider}</span>
            </div>
            <div className="activity-col-bar">
              <div
                className="activity-col-bar-fill"
                style={{ width: `${pct}%` }}
              />
              {col.errors > 0 && (
                <div
                  className="activity-col-bar-error"
                  style={{
                    width: `${Math.round((col.errors / col.total) * 100)}%`
                  }}
                />
              )}
            </div>
            <div className="activity-col-stats">
              <span className="text-[var(--gtm-success)]">
                <Check className="size-2.5 inline" /> {col.complete} complete
              </span>
              {col.running > 0 && (
                <span className="text-[var(--gtm-warning)]">
                  <Loader2 className="size-2.5 inline animate-spin" />{" "}
                  {col.running} running
                </span>
              )}
              {col.errors > 0 && (
                <span className="text-[var(--gtm-danger)]">
                  <AlertCircle className="size-2.5 inline" /> {col.errors}{" "}
                  failed
                </span>
              )}
              {col.skipped > 0 && (
                <span className="text-muted-foreground">
                  {col.skipped} skipped
                </span>
              )}
              <span className="text-muted-foreground">
                <Clock className="size-2.5 inline" /> {col.pending} not run /
                pending
              </span>
              <span className="text-muted-foreground ml-auto tabular-nums">
                {pct}% complete
              </span>
            </div>
          </div>
        )
      })}
    </div>
  )
}

// ── Errors Tab ───────────────────────────────────────────────────────────

function ErrorsTab({
  errors
}: {
  errors: ReturnType<typeof workbookActivityErrors>
}) {
  const [page, setPage] = useState(0)
  const pageSize = 50
  const currentPage = Math.min(
    page,
    Math.max(0, Math.ceil(errors.length / pageSize) - 1)
  )
  const offset = currentPage * pageSize
  if (errors.length === 0) {
    return (
      <div className="activity-drawer-empty">
        <Check className="size-8 text-[var(--gtm-success)]/20" />
        <p>No errors in loaded rows</p>
        <p className="text-xs text-muted-foreground">
          No loaded cells have an error status.
        </p>
      </div>
    )
  }

  return (
    <div className="activity-drawer-feed">
      <div className="flex flex-wrap items-center justify-between gap-2 py-2 text-xs text-muted-foreground">
        <span role="status">
          {offset + 1}–{Math.min(offset + pageSize, errors.length)} of{" "}
          {errors.length.toLocaleString()} errors in loaded rows
        </span>
        {errors.length > pageSize && (
          <div className="flex gap-2">
            <Button
              size="sm"
              disabled={currentPage === 0}
              onClick={() => setPage(currentPage - 1)}
              aria-label="Previous error page"
            >
              Previous
            </Button>
            <Button
              size="sm"
              disabled={offset + pageSize >= errors.length}
              onClick={() => setPage(currentPage + 1)}
              aria-label="Next error page"
            >
              Next
            </Button>
          </div>
        )}
      </div>
      {errors.slice(offset, offset + pageSize).map((ev) => (
        <div key={ev.id} className="activity-event enrich_error">
          <AlertCircle className="size-3 text-[var(--gtm-danger)] shrink-0" />
          <span className="activity-event-company">{ev.company}</span>
          <span className="activity-event-column">{ev.column}</span>
          {ev.provider && (
            <span className="activity-event-provider">{ev.provider}</span>
          )}
          <span className="activity-event-error">{ev.error}</span>
        </div>
      ))}
    </div>
  )
}
