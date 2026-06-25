// React Query hooks for the GTM Automation UI (Outreach · Automations · Watches)
// plus the meta hooks (useRole / useFlags) that back proactive gating.
//
// Pattern (spec §5, style #1): fetch fns in api.ts → hooks here → keys in
// query-client.ts. Every mutation attaches a shared `onError` that maps ApiError
// status → a human message (toast), so nothing fails silently even if a caller
// forgets a handler. Components may still pass their own `onError`/`onSuccess`
// for inline/field-level handling — both run.

import {
  useQuery,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query"
import { toast } from "sonner"
import { queryKeys } from "./query-client"
import {
  ApiError,
  fetchMeContext,
  fetchFlags,
  // outreach
  listSequences,
  getSequence,
  createSequence,
  updateSequence,
  deleteSequence,
  startSequence,
  pauseSequence,
  enrollLeads,
  executeSequence,
  listSends,
  getSmtpStatus,
  updateSmtp,
  testSmtp,
  listSuppressions,
  addSuppression,
  removeSuppression,
  // automations
  listTriggers,
  getTrigger,
  createTrigger,
  patchTrigger,
  deleteTrigger,
  pauseTrigger,
  resumeTrigger,
  previewTrigger,
  runTrigger,
  listRuns,
  getRun,
  // watches
  listWatches,
  getWatch,
  createWatch,
  patchWatch,
  deleteWatch,
  pollWatch,
  listWatchSignals,
  type WorkspaceRole,
  type FeatureFlags,
  type CreateSequenceRequest,
  type EnrollLeadsRequest,
  type CreateTriggerRequest,
  type TriggerType,
  type TriggerRun,
  type WatchCreate,
} from "./api"

// ── Shared error mapping ────────────────────────────────────────────────────

/** Map an ApiError (or unknown) to a user-facing message per spec §6/§7. */
export function apiErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return err instanceof Error ? err.message : "Something went wrong"
  }
  const d = err.detail
  switch (err.status) {
    case 403:
      // Locked-suppression 403 carries its own descriptive detail; role 403
      // uses the generic "Insufficient workspace role".
      if (/unsubscribe|complaint/i.test(d)) {
        return "Locked (unsubscribe/complaint) and cannot be removed."
      }
      return d && !/insufficient/i.test(d)
        ? d
        : "This action requires a higher workspace role (admin/editor)."
    case 409:
      return d || "Conflict"
    case 404:
      return d || "Not found"
    case 429:
      return d || "Rate limited — try again shortly."
    case 422:
    case 400:
      return d || "Invalid request"
    default:
      return d || `Request failed (${err.status})`
  }
}

/** Default toast-on-error handler shared by every mutation hook. */
function toastError(err: unknown) {
  toast.error(apiErrorMessage(err))
}

/**
 * Caller-supplied mutation callbacks (lightweight, decoupled from TanStack's
 * exact callback arity so forwarding stays stable across versions). Components
 * pass these for inline/field-level handling; the shared toast still fires.
 */
export interface MutOpts<TData, _TError, TVars> {
  onSuccess?: (data: TData, vars: TVars, ctx: unknown) => void
  onError?: (err: unknown, vars: TVars, ctx: unknown) => void
}

/** Merge the shared onError toast with any caller-supplied onError. */
function withErrorToast<TData, TError, TVars>(opts?: MutOpts<TData, TError, TVars>) {
  return {
    onError: (err: unknown, vars: TVars, ctx: unknown) => {
      toastError(err)
      opts?.onError?.(err, vars, ctx)
    },
  }
}

// ── Meta: role + flags ──────────────────────────────────────────────────────

export interface RoleInfo {
  role: WorkspaceRole | "unknown"
  isOwner: boolean
  /** Owner/admin satisfy admin-only mutations. */
  canAdmin: boolean
  /** Owner/admin/editor satisfy editor-or-admin mutations. */
  canEdit: boolean
  isLoading: boolean
}

export function useRole(): RoleInfo {
  const q = useQuery({
    queryKey: queryKeys.meta.context,
    queryFn: fetchMeContext,
    staleTime: 60 * 1000,
    retry: 0,
  })
  const role = q.data?.role ?? "unknown"
  const isOwner = q.data?.is_owner ?? false
  const canAdmin = role === "owner" || role === "admin"
  const canEdit = canAdmin || role === "editor"
  return { role, isOwner, canAdmin, canEdit, isLoading: q.isLoading }
}

export function useFlags() {
  return useQuery<FeatureFlags>({
    queryKey: queryKeys.meta.flags,
    queryFn: fetchFlags,
    staleTime: 60 * 1000,
    retry: 0,
  })
}

// ════════════════════════════════ Outreach ═════════════════════════════════

export function useSequences() {
  return useQuery({
    queryKey: queryKeys.outreach.sequences,
    queryFn: listSequences,
  })
}

export function useSequence(id: string | null) {
  return useQuery({
    queryKey: id ? queryKeys.outreach.sequence(id) : ["outreach", "sequence", "none"],
    queryFn: () => getSequence(id as string),
    enabled: !!id,
  })
}

export function useSequenceSends(id: string | null) {
  return useQuery({
    queryKey: id ? queryKeys.outreach.sends(id) : ["outreach", "sends", "none"],
    queryFn: () => listSends(id as string),
    enabled: !!id,
  })
}

export function useSmtpStatus() {
  return useQuery({ queryKey: queryKeys.outreach.smtp, queryFn: getSmtpStatus })
}

export function useSuppressions() {
  return useQuery({
    queryKey: queryKeys.outreach.suppressions,
    queryFn: listSuppressions,
  })
}

function useInvalidateSequence() {
  const qc = useQueryClient()
  return (id?: string) => {
    qc.invalidateQueries({ queryKey: queryKeys.outreach.sequences })
    if (id) qc.invalidateQueries({ queryKey: queryKeys.outreach.sequence(id) })
  }
}

export function useCreateSequence(
  opts?: MutOpts<unknown, unknown, CreateSequenceRequest>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: (b: CreateSequenceRequest) => createSequence(b),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate()
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useUpdateSequence(
  opts?: MutOpts<unknown, unknown, { id: string; body: Partial<CreateSequenceRequest> & { status?: string } }>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: Partial<CreateSequenceRequest> & { status?: string } }) =>
      updateSequence(id, body),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate(vars.id)
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useDeleteSequence(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: (id: string) => deleteSequence(id),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate()
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useStartSequence(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: (id: string) => startSequence(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate(id)
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

export function usePauseSequence(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: (id: string) => pauseSequence(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate(id)
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

export function useEnrollLeads(
  opts?: MutOpts<unknown, unknown, { id: string; body: EnrollLeadsRequest }>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: EnrollLeadsRequest }) => enrollLeads(id, body),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate(vars.id)
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useExecuteSequence(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateSequence()
  return useMutation({
    mutationFn: (id: string) => executeSequence(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate(id)
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

export function useUpdateSmtp(
  opts?: MutOpts<unknown, unknown, Record<string, unknown>>,
) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: Record<string, unknown>) => updateSmtp(b),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      qc.invalidateQueries({ queryKey: queryKeys.outreach.smtp })
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useTestSmtp(
  opts?: MutOpts<unknown, unknown, string>,
) {
  return useMutation({
    mutationFn: (to_email: string) => testSmtp(to_email),
    ...withErrorToast(opts),
  })
}

export function useAddSuppression(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (email: string) => addSuppression(email),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      qc.invalidateQueries({ queryKey: queryKeys.outreach.suppressions })
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useRemoveSuppression(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (email: string) => removeSuppression(email),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      qc.invalidateQueries({ queryKey: queryKeys.outreach.suppressions })
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

// ══════════════════════════════ Automations ════════════════════════════════

export function useTriggers(filters?: { enabled?: boolean; trigger_type?: TriggerType }) {
  return useQuery({
    queryKey: queryKeys.automations.triggers(filters),
    queryFn: () => listTriggers(filters),
    retry: 0, // 404 (flag-off) should surface immediately, not retry
  })
}

export function useTrigger(id: string | null) {
  return useQuery({
    queryKey: id ? queryKeys.automations.trigger(id) : ["automations", "trigger", "none"],
    queryFn: () => getTrigger(id as string),
    enabled: !!id,
  })
}

/** Runs list; polls while any run is queued/running (spec §5). */
export function useRuns(id: string | null) {
  return useQuery({
    queryKey: id ? queryKeys.automations.runs(id) : ["automations", "runs", "none"],
    queryFn: () => listRuns(id as string),
    enabled: !!id,
    refetchInterval: (query) => {
      const runs = query.state.data as TriggerRun[] | undefined
      const active = runs?.some((r) => r.status === "queued" || r.status === "running")
      return active ? 4000 : false
    },
  })
}

export function useRun(runId: string | null) {
  return useQuery({
    queryKey: runId ? queryKeys.automations.run(runId) : ["automations", "run", "none"],
    queryFn: () => getRun(runId as string),
    enabled: !!runId,
    refetchInterval: (query) => {
      const run = query.state.data as TriggerRun | undefined
      return run && (run.status === "queued" || run.status === "running") ? 4000 : false
    },
  })
}

function useInvalidateTrigger() {
  const qc = useQueryClient()
  return (id?: string) => {
    qc.invalidateQueries({ queryKey: queryKeys.automations.all })
    if (id) {
      qc.invalidateQueries({ queryKey: queryKeys.automations.trigger(id) })
      qc.invalidateQueries({ queryKey: queryKeys.automations.runs(id) })
    }
  }
}

export function useCreateTrigger(
  opts?: MutOpts<unknown, unknown, CreateTriggerRequest>,
) {
  const invalidate = useInvalidateTrigger()
  return useMutation({
    mutationFn: (b: CreateTriggerRequest) => createTrigger(b),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate()
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function usePatchTrigger(
  opts?: MutOpts<unknown, unknown, { id: string; body: Partial<CreateTriggerRequest> }>,
) {
  const invalidate = useInvalidateTrigger()
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: Partial<CreateTriggerRequest> }) =>
      patchTrigger(id, body),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate(vars.id)
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useDeleteTrigger(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateTrigger()
  return useMutation({
    mutationFn: (id: string) => deleteTrigger(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate()
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

export function usePauseTrigger(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateTrigger()
  return useMutation({
    mutationFn: (id: string) => pauseTrigger(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate(id)
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

export function useResumeTrigger(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateTrigger()
  return useMutation({
    mutationFn: (id: string) => resumeTrigger(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate(id)
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

/** Preview is a mutation (re-computes each call), never cached (spec §5). */
export function usePreviewTrigger(
  opts?: MutOpts<import("./api").PreviewResult, unknown, { id: string; body: { row_ids?: string[]; limit?: number } }>,
) {
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: { row_ids?: string[]; limit?: number } }) =>
      previewTrigger(id, body),
    ...withErrorToast(opts),
  })
}

export function useRunTrigger(
  opts?: MutOpts<import("./api").RunHandle, unknown, { id: string; body: { row_ids?: string[]; dry_run: boolean } }>,
) {
  const invalidate = useInvalidateTrigger()
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: { row_ids?: string[]; dry_run: boolean } }) =>
      runTrigger(id, body),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate(vars.id)
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

// ════════════════════════════════ Watches ══════════════════════════════════

export function useWatches(p?: { limit?: number; offset?: number }) {
  return useQuery({
    queryKey: queryKeys.watches.list(p),
    queryFn: () => listWatches(p),
    retry: 0, // 404/409 (flag-off / PG dependency) should surface immediately
  })
}

export function useWatch(id: string | null) {
  return useQuery({
    queryKey: id ? queryKeys.watches.detail(id) : ["watches", "detail", "none"],
    queryFn: () => getWatch(id as string),
    enabled: !!id,
  })
}

export function useWatchSignals(id: string | null, p?: { signal_type?: string; limit?: number; offset?: number }) {
  return useQuery({
    queryKey: id ? queryKeys.watches.signals(id) : ["watches", "signals", "none"],
    queryFn: () => listWatchSignals(id as string, p),
    enabled: !!id,
  })
}

function useInvalidateWatch() {
  const qc = useQueryClient()
  return (id?: string) => {
    qc.invalidateQueries({ queryKey: queryKeys.watches.all })
    if (id) {
      qc.invalidateQueries({ queryKey: queryKeys.watches.detail(id) })
      qc.invalidateQueries({ queryKey: queryKeys.watches.signals(id) })
    }
  }
}

export function useCreateWatch(
  opts?: MutOpts<unknown, unknown, WatchCreate>,
) {
  const invalidate = useInvalidateWatch()
  return useMutation({
    mutationFn: (b: WatchCreate) => createWatch(b),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate()
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function usePatchWatch(
  opts?: MutOpts<unknown, unknown, { id: string; body: Partial<WatchCreate> & { enabled?: boolean } }>,
) {
  const invalidate = useInvalidateWatch()
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: Partial<WatchCreate> & { enabled?: boolean } }) =>
      patchWatch(id, body),
    ...withErrorToast(opts),
    onSuccess: (data, vars, ctx) => {
      invalidate(vars.id)
      opts?.onSuccess?.(data, vars, ctx)
    },
  })
}

export function useDeleteWatch(
  opts?: MutOpts<unknown, unknown, string>,
) {
  const invalidate = useInvalidateWatch()
  return useMutation({
    mutationFn: (id: string) => deleteWatch(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate()
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}

export function usePollWatch(
  opts?: MutOpts<import("./api").PollResult, unknown, string>,
) {
  const invalidate = useInvalidateWatch()
  return useMutation({
    mutationFn: (id: string) => pollWatch(id),
    ...withErrorToast(opts),
    onSuccess: (data, id, ctx) => {
      invalidate(id)
      opts?.onSuccess?.(data, id, ctx)
    },
  })
}
