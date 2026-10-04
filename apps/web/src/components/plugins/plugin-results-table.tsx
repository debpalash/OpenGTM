// Results of one plugin run: a page of records (offset/limit) whose columns
// are the union of the records' `data` keys. Rows are virtualized (a page can
// hold up to 500 records) and expand to show their evidence.

import { useMemo, useRef, useState } from "react"
import { type ColumnDef, flexRender, getCoreRowModel, useReactTable } from "@tanstack/react-table"
import { useVirtualizer } from "@tanstack/react-virtual"
import { ChevronRight, ExternalLink } from "lucide-react"
import { Button } from "@/components/ui/button"
import { NativeSelect } from "@/components/ui/native-select"
import { humanize } from "@/lib/json-schema-form"
import type { PluginResult } from "@/lib/plugin-api"
import { evidenceSummary, normalizeEvidence, safeHttpUrl } from "@/lib/plugin-evidence"
import { cellText, resultColumns } from "@/lib/plugin-format"
import { cn } from "@/lib/utils"
import { EvidencePanel } from "./evidence-panel"

export const RESULT_PAGE_SIZES = [50, 100, 500] as const

function Cell({ value }: { value: unknown }) {
  const text = cellText(value)
  if (!text) return <span className="text-muted-foreground">—</span>
  const href = safeHttpUrl(value)
  if (href) {
    return <a href={href} target="_blank" rel="noopener noreferrer nofollow" onClick={event => event.stopPropagation()}
      className="inline-flex max-w-full items-center gap-1 text-[var(--gtm-accent)] hover:underline" title={text}>
      <span className="truncate">{text.replace(/^https?:\/\//, "")}</span><ExternalLink aria-hidden="true" className="size-3 shrink-0" />
    </a>
  }
  return <span className={cn("block truncate", typeof value === "object" && "font-mono text-[11px]")} title={text}>{text}</span>
}

export function PluginResultsTable({ results, total, offset, limit, loading, onPage, onLimit }: {
  results: PluginResult[]; total: number; offset: number; limit: number; loading: boolean
  onPage: (offset: number) => void; onLimit: (limit: number) => void
}) {
  const [expanded, setExpanded] = useState<ReadonlySet<number>>(() => new Set())
  const scrollRef = useRef<HTMLDivElement>(null)
  const keys = useMemo(() => resultColumns(results), [results])
  const toggle = (index: number) => setExpanded(current => {
    const next = new Set(current)
    if (!next.delete(index)) next.add(index)
    return next
  })

  const columns = useMemo<ColumnDef<PluginResult>[]>(() => [
    {
      id: "expand", header: () => <span className="sr-only">Evidence</span>,
      cell: ({ row }) => {
        const open = expanded.has(row.original.index)
        return <Button type="button" variant="ghost" size="icon-xs" aria-expanded={open} aria-controls={`evidence-${row.original.index}`}
          aria-label={`${open ? "Hide" : "Show"} evidence for record ${row.original.index + 1}`}
          onClick={event => { event.stopPropagation(); toggle(row.original.index) }}>
          <ChevronRight aria-hidden="true" className={cn("motion-safe:transition-transform", open && "rotate-90")} />
        </Button>
      },
    },
    { id: "#", header: "#", cell: ({ row }) => <span className="tabular-nums text-muted-foreground">{row.original.index + 1}</span> },
    {
      id: "__source", header: "Source",
      cell: ({ row }) => <span className="block truncate text-muted-foreground">{evidenceSummary(normalizeEvidence(row.original.evidence))}</span>,
    },
    ...keys.map((key): ColumnDef<PluginResult> => ({
      id: `data:${key}`, header: () => <span title={key}>{humanize(key)}</span>, cell: ({ row }) => <Cell value={row.original.data[key]} />,
    })),
  ], [keys, expanded])

  const table = useReactTable({ data: results, columns, getCoreRowModel: getCoreRowModel(), getRowId: result => String(result.index) })
  const rows = table.getRowModel().rows
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 37,
    overscan: 12,
    getItemKey: index => rows[index]?.id ?? index,
  })
  const items = virtualizer.getVirtualItems()
  const padTop = items[0]?.start ?? 0
  const padBottom = items.length ? virtualizer.getTotalSize() - items[items.length - 1].end : 0
  const lastRecord = Math.min(total, offset + results.length)
  const colSpan = columns.length

  return (
    <div className="flex flex-col gap-2">
      <div ref={scrollRef} className="max-h-[70vh] overflow-auto rounded-lg border" role="region" aria-label="Run results" tabIndex={0} aria-busy={loading}>
        <table className="w-full text-xs" aria-rowcount={total + 1}>
          <thead className="sticky top-0 z-10 bg-background shadow-[0_1px_0_var(--border)]">
            {table.getHeaderGroups().map(group => <tr key={group.id}>
              {group.headers.map(header => <th key={header.id} scope="col"
                className={cn("h-9 px-2 text-left font-medium whitespace-nowrap", header.id === "expand" && "w-8", header.id === "#" && "w-10", header.id === "__source" && "w-40")}>
                {flexRender(header.column.columnDef.header, header.getContext())}
              </th>)}
            </tr>)}
          </thead>
          {padTop > 0 && <tbody aria-hidden="true"><tr><td colSpan={colSpan} style={{ height: padTop, padding: 0 }} /></tr></tbody>}
          {items.map(item => {
            const row = rows[item.index]
            const open = expanded.has(row.original.index)
            return <tbody key={item.key} data-index={item.index} ref={virtualizer.measureElement} className="border-b last:border-0">
              <tr aria-rowindex={row.original.index + 2} className={cn("cursor-pointer hover:bg-muted/50", open && "bg-muted/40")} onClick={() => toggle(row.original.index)}>
                {row.getVisibleCells().map(cell => <td key={cell.id} className={cn("h-9 max-w-xs px-2 align-middle whitespace-nowrap", cell.column.id === "__source" && "max-w-40")}>
                  {flexRender(cell.column.columnDef.cell, cell.getContext())}
                </td>)}
              </tr>
              {open && <tr id={`evidence-${row.original.index}`} className="bg-muted/40">
                <td colSpan={colSpan} className="px-4 pt-1 pb-4">
                  <div className="max-w-3xl"><EvidencePanel evidence={row.original.evidence} /></div>
                </td>
              </tr>}
            </tbody>
          })}
          {padBottom > 0 && <tbody aria-hidden="true"><tr><td colSpan={colSpan} style={{ height: padBottom, padding: 0 }} /></tr></tbody>}
        </table>
      </div>
      <nav aria-label="Result pages" className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <span role="status">{total === 0 ? "No records" : `Records ${(offset + 1).toLocaleString()}–${lastRecord.toLocaleString()} of ${total.toLocaleString()}`}</span>
        <label className="ml-auto flex items-center gap-1.5">Per page
          <NativeSelect size="sm" value={String(limit)} onChange={event => onLimit(Number(event.target.value))}>
            {RESULT_PAGE_SIZES.map(size => <option key={size} value={size}>{size}</option>)}
          </NativeSelect>
        </label>
        <Button variant="outline" size="sm" disabled={offset === 0 || loading} onClick={() => onPage(Math.max(0, offset - limit))}>Previous</Button>
        <Button variant="outline" size="sm" disabled={offset + limit >= total || loading} onClick={() => onPage(offset + limit)}>Next</Button>
      </nav>
    </div>
  )
}
