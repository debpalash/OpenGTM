// Recent plugin runs (TanStack Table). Rows open the run detail page.

import { useEffect, useMemo, useState } from "react"
import { Link, useNavigate } from "@tanstack/react-router"
import {
  type ColumnDef, type SortingState, flexRender, getCoreRowModel, getSortedRowModel, useReactTable,
} from "@tanstack/react-table"
import { ArrowDown, ArrowUp, ChevronRight, History } from "lucide-react"
import { Empty } from "@/components/states"
import { NativeSelect } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { isActiveRun, type PluginInfo, type PluginRun } from "@/lib/plugin-api"
import { pluginTitle } from "@/lib/plugin-catalog"
import { absoluteTime, formatDuration, relativeTime, runDurationMs } from "@/lib/plugin-format"
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

export function PluginRunsTable({ runs, plugins, highlightId }: {
  runs: PluginRun[]; plugins: Map<string, PluginInfo>; highlightId?: string | null
}) {
  const navigate = useNavigate()
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

  if (runs.length === 0) {
    return <Empty icon={History} title="No plugin runs yet" description="Run a plugin from the catalog. Its status, records and evidence show up here." />
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <NativeSelect aria-label="Status" value={status} onChange={event => setStatus(event.target.value)}>
          <option value="all">All statuses</option>
          {["pending", "running", "completed", "failed", "cancelled"].map(value => <option key={value} value={value}>{value}</option>)}
        </NativeSelect>
        <p className="ml-auto text-xs text-muted-foreground" role="status">
          {status === "all" ? `${runs.length} recent ${runs.length === 1 ? "run" : "runs"}` : `${visible.length} of ${runs.length} recent runs`}
        </p>
      </div>
      <div className="rounded-lg border">
        <Table aria-label="Plugin runs">
          <TableHeader>
            {table.getHeaderGroups().map(group => <TableRow key={group.id}>
              {group.headers.map(header => {
                const sorted = header.column.getIsSorted()
                return <TableHead key={header.id} aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : undefined}
                  className={cn("text-xs", header.id === "open" && "w-8")}>
                  {header.column.getCanSort()
                    ? <button type="button" onClick={header.column.getToggleSortingHandler()} className="inline-flex items-center gap-1 hover:text-foreground">
                      {flexRender(header.column.columnDef.header, header.getContext())}
                      {sorted === "asc" ? <ArrowUp aria-hidden="true" className="size-3" /> : sorted === "desc" ? <ArrowDown aria-hidden="true" className="size-3" /> : null}
                    </button>
                    : flexRender(header.column.columnDef.header, header.getContext())}
                </TableHead>
              })}
            </TableRow>)}
          </TableHeader>
          <TableBody>
            {table.getRowModel().rows.length === 0
              ? <TableRow><TableCell colSpan={columns.length} className="py-8 text-center text-xs text-muted-foreground">No runs with this status.</TableCell></TableRow>
              : table.getRowModel().rows.map(row => <TableRow key={row.id} data-run-id={row.id} data-state={row.id === highlightId ? "selected" : undefined}
                className="cursor-pointer" onClick={() => void navigate({ to: "/plugins/runs/$runId", params: { runId: row.id } })}>
                {row.getVisibleCells().map(cell => <TableCell key={cell.id} className={cn("text-xs", cell.column.id === "plugin" && "max-w-64")}>
                  {flexRender(cell.column.columnDef.cell, cell.getContext())}
                </TableCell>)}
              </TableRow>)}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}
