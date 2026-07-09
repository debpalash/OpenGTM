/**
 * Workbook React Query hooks — Hybrid model.
 *
 * Rows are leads. WebSocket updates enrichment overlays.
 */

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useCallback, useState } from "react"
import {
  fetchWorkbooks, fetchWorkbook, createWorkbook, updateWorkbook,
  deleteWorkbook, updateLeadField, importLeads, deleteLeads,
  runWorkbook, stopWorkbook, runWorkbookCell,
  fetchWorkbookViews, createWorkbookView, updateWorkbookView, deleteWorkbookView,
  fetchProviders, fetchFilterOptions, createWorkbookSocket,
  type Workbook, type WorkbookLeadRow, type ViewConfig,
} from "./workbook-api"

// ── Query Keys ───────────────────────────────────────────────────────────

export const workbookKeys = {
  all: ["workbooks"] as const,
  list: () => [...workbookKeys.all, "list"] as const,
  detail: (id: string) => [...workbookKeys.all, "detail", id] as const,
  views: (id: string) => [...workbookKeys.all, "views", id] as const,
  providers: () => [...workbookKeys.all, "providers"] as const,
  filterOptions: () => [...workbookKeys.all, "filter-options"] as const,
}

// ── Workbook List ────────────────────────────────────────────────────────

export function useWorkbooks() {
  return useQuery({
    queryKey: workbookKeys.list(),
    queryFn: fetchWorkbooks,
  })
}

// ── Workbook Detail + Leads ──────────────────────────────────────────────

export function useWorkbook(id: string, pageSize = 1000) {
  return useQuery({
    queryKey: workbookKeys.detail(id),
    queryFn: () => fetchWorkbook(id, 1, pageSize),
    enabled: !!id,
    // Don't retry client errors (404 not-found / 403 no-access won't resolve on
    // their own); only retry transient/server errors once.
    retry: (count, err) => {
      const status = (err as { status?: number })?.status
      if (status && status >= 400 && status < 500) return false
      return count < 1
    },
  })
}

// ── Filter Options (for workbook creation) ───────────────────────────────

export function useFilterOptions() {
  return useQuery({
    queryKey: workbookKeys.filterOptions(),
    queryFn: fetchFilterOptions,
    staleTime: 60 * 1000,
  })
}

// ── Mutations ────────────────────────────────────────────────────────────

export function useCreateWorkbook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: createWorkbook,
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.list() }),
  })
}

export function useUpdateWorkbook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, ...data }: Partial<Workbook> & { id: string }) => updateWorkbook(id, data),
    onSuccess: (_, vars) => {
      qc.invalidateQueries({ queryKey: workbookKeys.detail(vars.id) })
      qc.invalidateQueries({ queryKey: workbookKeys.list() })
    },
  })
}

export function useDeleteWorkbook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: deleteWorkbook,
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.list() }),
  })
}

/** Update a Lead field from the workbook editor */
export function useUpdateLeadField(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ leadId, fields }: { leadId: number; fields: Record<string, any> }) =>
      updateLeadField(workbookId, leadId, fields),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.detail(workbookId) }),
  })
}

/** Import CSV rows as new leads */
export function useImportLeads(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (rows: Record<string, any>[]) => importLeads(workbookId, rows),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.detail(workbookId) }),
  })
}

export function useRunWorkbook(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (opts?: { column_ids?: string[]; row_ids?: number[]; lead_ids?: number[]; fill_missing?: boolean; force?: boolean }) => runWorkbook(workbookId, opts),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.detail(workbookId) }),
  })
}

/** (Re-)run a single cell synchronously; force bypasses success-skip gates. */
export function useRunCell(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ rowId, colId, force }: { rowId: number; colId: string; force?: boolean }) =>
      runWorkbookCell(workbookId, rowId, colId, force ?? false),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.detail(workbookId) }),
  })
}

// ── Saved Views ──────────────────────────────────────────────────────────

export function useWorkbookViews(workbookId: string) {
  return useQuery({
    queryKey: workbookKeys.views(workbookId),
    queryFn: () => fetchWorkbookViews(workbookId),
    enabled: !!workbookId,
  })
}

export function useCreateView(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: { name: string; config?: Partial<ViewConfig> }) =>
      createWorkbookView(workbookId, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.views(workbookId) }),
  })
}

export function useUpdateView(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ viewId, ...body }: { viewId: string; name?: string; config?: ViewConfig }) =>
      updateWorkbookView(workbookId, viewId, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.views(workbookId) }),
  })
}

export function useDeleteView(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (viewId: string) => deleteWorkbookView(workbookId, viewId),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.views(workbookId) }),
  })
}

export function useStopWorkbook(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => stopWorkbook(workbookId),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.detail(workbookId) }),
  })
}

export function useDeleteLeads(workbookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (leadIds: number[]) => deleteLeads(leadIds),
    onSuccess: () => qc.invalidateQueries({ queryKey: workbookKeys.detail(workbookId) }),
  })
}

// ── Providers ────────────────────────────────────────────────────────────

export function useProviders() {
  return useQuery({
    queryKey: workbookKeys.providers(),
    queryFn: fetchProviders,
    staleTime: 60 * 1000,
  })
}

// ── WebSocket Hook ───────────────────────────────────────────────────────

export function useWorkbookSocket(workbookId: string | undefined) {
  const qc = useQueryClient()
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)

  // Batch cell updates: accumulate in a ref and flush once per animation frame
  const pendingUpdates = useRef<Map<string, any>>(new Map())
  const rafRef = useRef<number | null>(null)

  const flushUpdates = useCallback(() => {
    if (!workbookId || pendingUpdates.current.size === 0) return

    // Group by lead_id
    const byLead = new Map<string, any[]>()
    for (const [, msg] of pendingUpdates.current) {
      const lid = String(msg.leadId)
      if (!byLead.has(lid)) byLead.set(lid, [])
      byLead.get(lid)!.push(msg)
    }
    pendingUpdates.current.clear()
    rafRef.current = null

    qc.setQueryData(workbookKeys.detail(workbookId), (old: any) => {
      if (!old?.rows) return old
      return {
        ...old,
        rows: old.rows.map((row: WorkbookLeadRow) => {
          const updates = byLead.get(String(row.lead_id))
          if (!updates) return row // same reference — no re-render
          let newLead = row.lead
          let newEnrichments = { ...row.enrichments }
          for (const u of updates) {
            if (u.leadField) {
              newLead = { ...newLead, [u.leadField]: u.value }
            }
            newEnrichments[u.colId] = {
              value: u.value, status: u.status,
              provider: u.provider, error: u.error,
            }
          }
          return { ...row, lead: newLead, enrichments: newEnrichments }
        }),
      }
    })
  }, [workbookId, qc])

  useEffect(() => {
    if (!workbookId) return

    const ws = createWorkbookSocket(workbookId)
    wsRef.current = ws

    ws.onopen = () => {
      setConnected(true)
      const ping = setInterval(() => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "ping" }))
        }
      }, 30000)
      ws.addEventListener("close", () => clearInterval(ping))
    }

    ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data)

        if (msg.type === "cell_update") {
          // Accumulate in batch — don't trigger React re-render yet
          pendingUpdates.current.set(`${msg.leadId}:${msg.colId}`, msg)

          // Schedule flush on next animation frame (coalesces all updates in this frame)
          if (!rafRef.current) {
            rafRef.current = requestAnimationFrame(flushUpdates)
          }
        }

        if (msg.type === "workbook_status") {
          qc.setQueryData(workbookKeys.detail(workbookId), (old: any) => {
            if (!old) return old
            return { ...old, workbook: { ...old.workbook, status: msg.status } }
          })
        }
      } catch {
        // ignore bad messages
      }
    }

    ws.onclose = () => setConnected(false)
    ws.onerror = () => setConnected(false)

    return () => {
      ws.close()
      wsRef.current = null
      setConnected(false)
      if (rafRef.current) cancelAnimationFrame(rafRef.current)
    }
  }, [workbookId, qc, flushUpdates])

  const send = useCallback((msg: Record<string, any>) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg))
    }
  }, [])

  return { connected, send }
}
