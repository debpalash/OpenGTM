// Plugin runs, newest first (TanStack Table). Rows open the run detail page.
//
// The list is paged on the server (keyset cursor). Pages append as you scroll
// near the end ("infinite scrolling") and a "Load more" button does the same
// for keyboard and screen-reader users. Only the rows in view are rendered
// (TanStack Virtual), so hundreds of loaded runs cost no more than a screenful.
// Sorting and the status filter apply to the runs loaded so far.

import { useEffect, useMemo, useRef, useState } from "react"
import { Link, useNavigate } from "@tanstack/react-router"
import {
  type ColumnDef, type SortingState, flexRender, getCoreRowModel, getSortedRowModel, useReactTable,
} from "@tanstack/react-table"
import { useVirtualizer } from "@tanstack/react-virtual"
import { ArrowDown, ArrowUp, ChevronRight, History, Loader2 } from "lucide-react"
import { Empty } from "@/components/states"
import { Button } from "@/components/ui/button"
import { NativeSelect } from "@/components/ui/native-select"
import { isActiveRun, type PluginInfo, type PluginRun } from "@/lib/plugin-api"
import { pluginTitle } from "@/lib/plugin-catalog"
import { absoluteTime, formatDuration, relativeTime, runDurationMs } from "@/lib/plugin-format"
import { shouldLoadMore } from "@/lib/plugin-run-list"
import { cn } from "@/lib/utils"
import { RunStatusBadge } from "./run-status-badge"

/** Re-render once a second while something is running, for live durations. */
function useNow(active: boolean) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [active])
  return now
}

export function PluginRunsTable({ runs, plugins, highlightId, hasMore = false, loadingMore = false, loadMoreFailed = false, onLoadMore }: {
  runs: PluginRun[]; plugins: Map<string, PluginInfo>; highlightId?: string | null
  /** The server has older runs than the ones loaded. */
  hasMore?: boolean
  loadingMore?: boolean
  /** The last request for another page failed; scrolling stops retrying until the button is used. */
  loadMoreFailed?: boolean
  onLoadMore?: () => void
}) {
  const navigate = useNavigate()
  const scrollRef = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState("all")
  const [sorting, setSorting] = useState<SortingState>([])
  const now = useNow(runs.some(isActiveRun))
  const visible = useMemo(() => status === "all" ? runs : runs.filter(run => run.status === status), [runs, status])

  const columns = useMemo<ColumnDef<PluginRun>[]>(() => [
    {
      id: "status", header: "Status", accessorKey: "status", enableSorting: false,
      cell: ({ row }) => <RunStatusBadge status={row.original.status} />,
    },
    {
      id: "plugin", header: "Plugin", accessorFn: run => plugins.get(run.plugin) ? pluginTitle(plugins.get(run.plugin)!) : run.plugin,
      cell: ({ row, getValue }) => <Link to="/plugins/runs/$runId" params={{ runId: row.original.id }} onClick={event => event.stopPropagation()}
        className="flex min-w-0 flex-col font-medium hover:underline focus-visible:underline">
        <span className="truncate">{getValue<string>()}</span>
        <span className="truncate font-mono text-[11px] font-normal text-muted-foreground">{row.original.plugin}{row.original.plugin_version && ` · v${row.original.plugin_version}`}</span>
      </Link>,
    },
    { id: "created_by", header: "Created by", accessorFn: run => run.created_by ?? "", cell: ({ getValue }) => getValue<string>() || <span className="text-muted-foreground">—</span> },
    {
      id: "created_at", header: "Created", accessorFn: run => Date.parse(run.created_at) || 0, sortDescFirst: true,
      cell: ({ row }) => <time dateTime={row.original.created_at} title={absoluteTime(row.original.created_at)}>{relativeTime(row.original.created_at, now)}</time>,
    },
    {
      id: "duration", header: "Duration", accessorFn: run => runDurationMs(run, now) ?? -1, sortDescFirst: true,
      cell: ({ row }) => <span className="tabular-nums">{formatDuration(runDurationMs(row.original, now))}</span>,
    },
    {
      id: "records", header: "Records", accessorFn: run => run.stats?.records ?? -1, sortDescFirst: true,
      cell: ({ row }) => {
        const stats = row.original.stats
        return <span className="tabular-nums">{stats?.records != null ? stats.records.toLocaleString() : "—"}
          {stats?.pages != null && stats.pages > 0 && <span className="ml-1 text-[11px] text-muted-foreground">({stats.pages} {stats.pages === 1 ? "page" : "pages"})</span>}
        </span>
      },
    },
    { id: "open", header: () => <span className="sr-only">Open</span>, enableSorting: false, cell: () => <ChevronRight aria-hidden="true" className="size-4 text-muted-foreground" /> },
  ], [plugins, now])

  const table = useReactTable({
    data: visible, columns, state: { sorting }, onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(), getSortedRowModel: getSortedRowModel(), getRowId: run => run.id,
  })

  const rows = table.getRowModel().rows
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 53,
    overscan: 10,
    getItemKey: index => rows[index]?.id ?? index,
  })
  const items = virtualizer.getVirtualItems()
  const padTop = items[0]?.start ?? 0
  const padBottom = items.length ? virtualizer.getTotalSize() - items[items.length - 1].end : 0

  // Infinite scrolling. Only for the unfiltered list: a filter that matches
  // few of the loaded runs would otherwise page through the whole history
  // looking for matches; there the button loads on request.
  const lastVisibleIndex = items.length ? items[items.length - 1].index : -1
  const autoLoad = status === "all" && !loadMoreFailed && !!onLoadMore
  useEffect(() => {
    if (autoLoad && shouldLoadMore({ lastVisibleIndex, count: rows.length, hasNextPage: hasMore, busy: loadingMore })) onLoadMore()
  }, [autoLoad, lastVisibleIndex, rows.length, hasMore, loadingMore, onLoadMore])

  if (runs.length === 0) {
    return <Empty icon={History} title="No plugin runs yet" description="Run a plugin from the catalog. Its status, records and evidence show up here." />
  }

  const loaded = `${runs.length}${hasMore ? "+" : ""}`
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <NativeSelect aria-label="Status" value={status} onChange={event => setStatus(event.target.value)}>
          <option value="all">All statuses</option>
          {["pending", "running", "completed", "failed", "cancelled"].map(value => <option key={value} value={value}>{value}</option>)}
        </NativeSelect>
        <p className="ml-auto text-xs text-muted-foreground" role="status">
          {status === "all" ? `${loaded} ${runs.length === 1 && !hasMore ? "run" : "runs"}` : `${visible.length} of ${loaded} loaded runs`}
        </p>
      </div>
      <div ref={scrollRef} className="max-h-[70vh] overflow-auto rounded-lg border" role="region" aria-label="Plugin runs" tabIndex={0}>
        <table className="w-full caption-bottom text-sm" aria-label="Plugin runs" aria-rowcount={rows.length + 1}>
          <thead className="sticky top-0 z-10 bg-background shadow-[0_1px_0_var(--border)]">
            {table.getHeaderGroups().map(group => <tr key={group.id}>
              {group.headers.map(header => {
                const sorted = header.column.getIsSorted()
                return <th key={header.id} scope="col" aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : undefined}
                  className={cn("h-10 px-2 text-left align-middle text-xs font-medium whitespace-nowrap text-foreground", header.id === "open" && "w-8")}>
                  {header.column.getCanSort()
                    ? <button type="button" onClick={header.column.getToggleSortingHandler()} className="inline-flex items-center gap-1 hover:text-foreground">
                      {flexRender(header.column.columnDef.header, header.getContext())}
                      {sorted === "asc" ? <ArrowUp aria-hidden="true" className="size-3" /> : sorted === "desc" ? <ArrowDown aria-hidden="true" className="size-3" /> : null}
                    </button>
                    : flexRender(header.column.columnDef.header, header.getContext())}
                </th>
              })}
            </tr>)}
          </thead>
          {rows.length === 0
            ? <tbody><tr><td colSpan={columns.length} className="py-8 text-center text-xs text-muted-foreground">
              No loaded runs have this status.{hasMore && " Load more to look further back."}
            </td></tr></tbody>
            : <>
              {padTop > 0 && <tbody aria-hidden="true"><tr><td colSpan={columns.length} style={{ height: padTop, padding: 0 }} /></tr></tbody>}
              {items.map(item => {
                const row = rows[item.index]
                return <tbody key={item.key} data-index={item.index} ref={virtualizer.measureElement}>
                  <tr aria-rowindex={item.index + 2} data-run-id={row.id} data-state={row.id === highlightId ? "selected" : undefined}
                    className="cursor-pointer border-b transition-colors hover:bg-muted/50 data-[state=selected]:bg-muted"
                    onClick={() => void navigate({ to: "/plugins/runs/$runId", params: { runId: row.id } })}>
                    {row.getVisibleCells().map(cell => <td key={cell.id} className={cn("p-2 align-middle text-xs whitespace-nowrap", cell.column.id === "plugin" && "max-w-64")}>
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>)}
                  </tr>
                </tbody>
              })}
              {padBottom > 0 && <tbody aria-hidden="true"><tr><td colSpan={columns.length} style={{ height: padBottom, padding: 0 }} /></tr></tbody>}
            </>}
        </table>
      </div>
      {(hasMore || loadMoreFailed) && onLoadMore && <div className="flex items-center justify-center gap-2">
        {loadMoreFailed && <p role="alert" className="text-xs text-destructive">Could not load more runs.</p>}
        <Button type="button" variant="outline" size="sm" disabled={loadingMore} onClick={onLoadMore}>
          {loadingMore ? <><Loader2 aria-hidden="true" className="animate-spin" />Loading…</> : loadMoreFailed ? "Try again" : "Load more"}
        </Button>
      </div>}
      {!hasMore && runs.length > 0 && <p className="text-center text-[11px] text-muted-foreground">That is every run.</p>}
    </div>
  )
}
