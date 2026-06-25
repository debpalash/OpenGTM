// Automations (rules) + run detail slice (spec §1.2).
//
// View machine (mirrors outreach.tsx): list | create | edit | detail | run.
//   * AUTOMATIONS_ENABLED off → /api/automations/* 404 → <FeatureDisabled flag-off>
//     (also proactively via useFlags, to avoid a flash).
//   * TriggerList: useTriggers + enable/pause/delete (admin-gated).
//   * RuleBuilder: name · trigger (on_signal|on_row_changed|on_row_added|
//     on_schedule[interval]) · scope workbooks (useWorkbooks) · condition
//     (ConditionFieldPicker) · ordered actions (ActionRow) · caps · dry-run
//     preview (client-side spend-cap compare, NO server 402) · Save / Save & Run.
//   * Save & Run returns a fire_key handle → navigate to the rule's run history,
//     match the new TriggerRun by fire_key (fall back to the runs list).
//   * TriggerDetail: recent runs (useRuns, polls while running).
//   * RunDetail: useRun (polls while running) + action_results table.

import { useEffect, useMemo, useState } from "react"
import {
  AlertTriangle,
  ArrowLeft,
  Pause,
  Play,
  Plus,
  Trash2,
  Zap,
} from "lucide-react"
import { toast } from "sonner"

import {
  ConditionFieldPicker,
  CONDITION_OPERATORS,
  ActionRow,
} from "@/components/builder-primitives"
import { ConfirmDialog } from "@/components/confirm-dialog"
import {
  FeatureDisabled,
  FeatureDisabledFromError,
  isFeatureDisabledError,
} from "@/components/feature-disabled"
import { Empty, ErrorState, Loading } from "@/components/states"
import { StatusDot } from "@/components/status-dot"
import { Gate, useCanRole } from "@/components/gate"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Separator } from "@/components/ui/separator"
import { Switch } from "@/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"

import {
  apiErrorMessage,
  useCreateTrigger,
  useDeleteTrigger,
  useFlags,
  usePatchTrigger,
  usePauseTrigger,
  usePreviewTrigger,
  useResumeTrigger,
  useRun,
  useRunTrigger,
  useRuns,
  useTrigger,
  useTriggers,
} from "@/lib/automation-hooks"
import {
  DEFAULT_ACTION_TYPES,
  LEGACY_ACTION_TYPES,
  type ActionType,
  type CreateTriggerRequest,
  type PreviewResult,
  type RuleAction,
  type Trigger,
  type TriggerRun,
  type TriggerType,
} from "@/lib/api"
import { useWorkbooks } from "@/lib/workbook-hooks"

const FEATURE = "Automations"

// ── Labels / small helpers ──────────────────────────────────────────────────

const TRIGGER_TYPE_LABELS: Record<TriggerType, string> = {
  on_signal: "On signal",
  on_row_changed: "On row changed",
  on_row_added: "On row added",
  on_schedule: "On schedule",
}

const ACTION_TYPE_LABELS: Record<ActionType, string> = {
  re_enrich: "Re-enrich",
  push_crm: "Push to CRM",
  webhook: "Webhook",
  sequencer: "Sequencer (legacy)",
  send_email: "Send email (legacy)",
}

const SCHEDULE_INTERVALS = ["hourly", "daily", "weekly"] as const
const CRM_TYPES = ["hubspot", "salesforce", "pipedrive"] as const
const WEBHOOK_METHODS = ["POST", "PUT", "GET"] as const

const usd = (n: number) =>
  `$${(Number.isFinite(n) ? n : 0).toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}`

function relativeTime(iso: string | null): string {
  if (!iso) return "—"
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return "—"
  const diff = Date.now() - t
  const min = Math.round(diff / 60000)
  if (min < 1) return "just now"
  if (min < 60) return `${min}m ago`
  const hr = Math.round(min / 60)
  if (hr < 24) return `${hr}h ago`
  return `${Math.round(hr / 24)}d ago`
}

const isRunActive = (s: string) => s === "queued" || s === "running"

// ════════════════════════════════ Page ═════════════════════════════════════

type View =
  | { name: "list" }
  | { name: "create" }
  | { name: "edit"; triggerId: string }
  | { name: "detail"; triggerId: string; pendingFireKey?: string }
  | { name: "run"; triggerId: string; runId: string }

export default function AutomationsPage() {
  const flags = useFlags()
  const triggers = useTriggers()
  const [view, setView] = useState<View>({ name: "list" })

  // Flag-off (404) → feature-disabled screen, driven by the page query error.
  if (isFeatureDisabledError(triggers.error)) {
    return <FeatureDisabledFromError error={triggers.error} feature={FEATURE} />
  }
  // Belt-and-suspenders: proactive flag check (avoids a flash before the query).
  if (flags.data && !flags.data.automations_enabled) {
    return <FeatureDisabled variant="flag-off" feature={FEATURE} />
  }

  if (view.name === "create") {
    return (
      <RuleBuilder
        onCancel={() => setView({ name: "list" })}
        onSaved={(id) => setView({ name: "detail", triggerId: id })}
        onRan={(id, fireKey) =>
          setView({ name: "detail", triggerId: id, pendingFireKey: fireKey })
        }
      />
    )
  }

  if (view.name === "edit") {
    return (
      <RuleBuilder
        editTriggerId={view.triggerId}
        onCancel={() => setView({ name: "detail", triggerId: view.triggerId })}
        onSaved={(id) => setView({ name: "detail", triggerId: id })}
        onRan={(id, fireKey) =>
          setView({ name: "detail", triggerId: id, pendingFireKey: fireKey })
        }
      />
    )
  }

  if (view.name === "detail") {
    return (
      <TriggerDetail
        triggerId={view.triggerId}
        pendingFireKey={view.pendingFireKey}
        onBack={() => setView({ name: "list" })}
        onEdit={() => setView({ name: "edit", triggerId: view.triggerId })}
        onOpenRun={(runId) =>
          setView({ name: "run", triggerId: view.triggerId, runId })
        }
      />
    )
  }

  if (view.name === "run") {
    return (
      <RunDetail
        runId={view.runId}
        onBack={() => setView({ name: "detail", triggerId: view.triggerId })}
      />
    )
  }

  // list
  return (
    <div className="flex flex-col gap-3 p-4">
      <header className="flex items-start justify-between gap-3">
        <div>
          <h1 className="text-base font-semibold">Automations</h1>
          <p className="text-xs text-muted-foreground">
            Rules that run actions when your data changes.
          </p>
        </div>
        <Gate need="admin">
          <Button
            size="sm"
            className="h-7 gap-1 text-xs"
            onClick={() => setView({ name: "create" })}
          >
            <Plus className="size-3" /> New rule
          </Button>
        </Gate>
      </header>
      <Separator />
      {triggers.isLoading ? (
        <Loading rows={4} />
      ) : triggers.isError ? (
        <ErrorState error={triggers.error} onRetry={() => triggers.refetch()} />
      ) : !triggers.data || triggers.data.length === 0 ? (
        <Empty
          icon={Zap}
          title="No rules yet"
          description="Create a rule to automatically enrich rows, push to your CRM, or call a webhook."
          action={
            <Gate need="admin">
              <Button
                size="sm"
                className="h-7 gap-1 text-xs"
                onClick={() => setView({ name: "create" })}
              >
                <Plus className="size-3" /> New rule
              </Button>
            </Gate>
          }
        />
      ) : (
        <TriggerList
          triggers={triggers.data}
          onOpen={(id) => setView({ name: "detail", triggerId: id })}
        />
      )}
    </div>
  )
}

// ════════════════════════════════ List ═════════════════════════════════════

function TriggerList({
  triggers,
  onOpen,
}: {
  triggers: Trigger[]
  onOpen: (id: string) => void
}) {
  const { allowed: canAdmin, reason } = useCanRole("admin")
  const pause = usePauseTrigger()
  const resume = useResumeTrigger()
  const del = useDeleteTrigger()
  const [confirmDelete, setConfirmDelete] = useState<Trigger | null>(null)

  return (
    <>
      <div className="flex flex-col gap-1.5">
        {triggers.map((t) => (
          <div
            key={t.id}
            className="flex items-center gap-3 rounded-lg border p-3 text-sm transition-colors hover:bg-muted/50"
          >
            <button
              type="button"
              className="flex min-w-0 flex-1 items-center gap-3 text-left"
              onClick={() => onOpen(t.id)}
            >
              <StatusDot status={t.enabled ? "enabled" : "disabled"} />
              <span className="min-w-0 flex-1 truncate font-medium">
                {t.name}
              </span>
              <Badge variant="secondary" className="text-[10px]">
                {TRIGGER_TYPE_LABELS[t.trigger_type] ?? t.trigger_type}
              </Badge>
              <span className="hidden w-24 shrink-0 text-right text-[11px] text-muted-foreground sm:inline">
                {t.last_fired_at
                  ? `fired ${relativeTime(t.last_fired_at)}`
                  : "never fired"}
              </span>
            </button>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    className="size-7"
                    aria-label={`Actions for ${t.name}`}
                  />
                }
              >
                ⋮
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => onOpen(t.id)}>
                  Open
                </DropdownMenuItem>
                {t.enabled ? (
                  <DropdownMenuItem
                    disabled={!canAdmin || pause.isPending}
                    onSelect={() => pause.mutate(t.id)}
                  >
                    <Pause className="size-3.5" /> Pause
                  </DropdownMenuItem>
                ) : (
                  <DropdownMenuItem
                    disabled={!canAdmin || resume.isPending}
                    onSelect={() => resume.mutate(t.id)}
                  >
                    <Play className="size-3.5" /> Enable
                  </DropdownMenuItem>
                )}
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  disabled={!canAdmin}
                  className="text-destructive"
                  onSelect={() => setConfirmDelete(t)}
                >
                  <Trash2 className="size-3.5" /> Delete
                </DropdownMenuItem>
                {!canAdmin && (
                  <p className="px-2 py-1 text-[10px] text-muted-foreground">
                    {reason}
                  </p>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        ))}
      </div>

      <ConfirmDialog
        open={!!confirmDelete}
        onOpenChange={(o) => !o && setConfirmDelete(null)}
        title="Delete rule?"
        description={
          confirmDelete
            ? `"${confirmDelete.name}" and its run history reference will be removed. This cannot be undone.`
            : undefined
        }
        destructive
        confirmLabel="Delete"
        pending={del.isPending}
        onConfirm={() => {
          if (!confirmDelete) return
          const id = confirmDelete.id
          del.mutate(id, {
            onSuccess: () => {
              toast.success("Rule deleted")
              setConfirmDelete(null)
            },
          })
        }}
      />
    </>
  )
}

// ═══════════════════════════════ Builder ════════════════════════════════════

/** UI-friendly action shape; serialized to RuleAction on save. */
interface UIAction {
  type: ActionType
  // re_enrich
  columnIds: string[]
  // push_crm
  crmType: string
  crmFields: { k: string; v: string }[]
  // webhook
  webhookUrl: string
  webhookMethod: string
  webhookBody: string
  webhookSecretRef: string
  // legacy (read-only passthrough)
  legacyConfig?: Record<string, unknown>
  readOnly?: boolean
}

function emptyAction(type: ActionType): UIAction {
  return {
    type,
    columnIds: [],
    crmType: "hubspot",
    crmFields: [],
    webhookUrl: "",
    webhookMethod: "POST",
    webhookBody: "",
    webhookSecretRef: "",
  }
}

function actionToUI(a: RuleAction, allowLegacy: boolean): UIAction {
  const c = a.config ?? {}
  const ui = emptyAction(a.type)
  if (a.type === "re_enrich") {
    ui.columnIds = Array.isArray(c.column_ids) ? (c.column_ids as string[]) : []
  } else if (a.type === "push_crm") {
    ui.crmType = typeof c.type === "string" ? c.type : "hubspot"
    const fm = (c.field_map ?? {}) as Record<string, unknown>
    ui.crmFields = Object.entries(fm).map(([k, v]) => ({ k, v: String(v) }))
  } else if (a.type === "webhook") {
    ui.webhookUrl = typeof c.url === "string" ? c.url : ""
    ui.webhookMethod = typeof c.method === "string" ? c.method : "POST"
    ui.webhookBody = c.body ? JSON.stringify(c.body, null, 2) : ""
    ui.webhookSecretRef =
      typeof c.header_secret_ref === "string" ? c.header_secret_ref : ""
  } else {
    // sequencer / send_email — keep config, render read-only when legacy is off
    ui.legacyConfig = c
    ui.readOnly = !allowLegacy
  }
  return ui
}

/** Returns a serialized action or an error string. */
function uiToAction(a: UIAction): { action?: RuleAction; error?: string } {
  switch (a.type) {
    case "re_enrich":
      if (a.columnIds.length === 0)
        return { error: "Re-enrich needs at least one column." }
      return { action: { type: a.type, config: { column_ids: a.columnIds } } }
    case "push_crm": {
      const field_map: Record<string, string> = {}
      for (const { k, v } of a.crmFields) {
        if (k.trim()) field_map[k.trim()] = v
      }
      return {
        action: { type: a.type, config: { type: a.crmType, field_map } },
      }
    }
    case "webhook": {
      const url = a.webhookUrl.trim()
      try {
        const u = new URL(url)
        if (u.protocol !== "http:" && u.protocol !== "https:")
          return { error: "Webhook URL must be http(s)." }
        if (u.username || u.password)
          return { error: "Webhook URL must not embed credentials." }
      } catch {
        return { error: "Webhook URL is not a valid URL." }
      }
      const config: Record<string, unknown> = { url, method: a.webhookMethod }
      if (a.webhookBody.trim()) {
        try {
          config.body = JSON.parse(a.webhookBody)
        } catch {
          return { error: "Webhook body must be valid JSON." }
        }
      }
      if (a.webhookSecretRef.trim())
        config.header_secret_ref = a.webhookSecretRef.trim()
      return { action: { type: a.type, config } }
    }
    default:
      return { action: { type: a.type, config: a.legacyConfig ?? {} } }
  }
}

function move<T>(arr: T[], from: number, to: number): T[] {
  if (to < 0 || to >= arr.length) return arr
  const copy = [...arr]
  const [item] = copy.splice(from, 1)
  copy.splice(to, 0, item)
  return copy
}

function RuleBuilder({
  editTriggerId,
  onCancel,
  onSaved,
  onRan,
}: {
  editTriggerId?: string
  onCancel: () => void
  onSaved: (id: string) => void
  onRan: (id: string, fireKey: string) => void
}) {
  const flags = useFlags()
  const allowLegacy = flags.data?.allow_legacy_outreach ?? false
  const workbooks = useWorkbooks()
  const existing = useTrigger(editTriggerId ?? null)
  const create = useCreateTrigger()
  const patch = usePatchTrigger()
  const preview = usePreviewTrigger()
  const run = useRunTrigger()
  const { allowed: canAdmin } = useCanRole("admin")

  // form state
  const [name, setName] = useState("")
  const [triggerType, setTriggerType] = useState<TriggerType>("on_row_added")
  const [interval, setIntervalVal] = useState<string>("daily")
  const [scopeIds, setScopeIds] = useState<string[]>([])
  const [condition, setCondition] = useState("")
  const [actions, setActions] = useState<UIAction[]>([])
  const [maxSpend, setMaxSpend] = useState<string>("")
  const [maxActions, setMaxActions] = useState<string>("")
  const [stopOnError, setStopOnError] = useState(true)

  // derived
  const [savedId, setSavedId] = useState<string | null>(editTriggerId ?? null)
  const [previewResult, setPreviewResult] = useState<PreviewResult | null>(null)
  const [conditionError, setConditionError] = useState<string | null>(null)
  const [runOpen, setRunOpen] = useState(false)
  const [hydrated, setHydrated] = useState(!editTriggerId)

  // hydrate when editing
  useEffect(() => {
    if (!editTriggerId || hydrated || !existing.data) return
    const t = existing.data
    setName(t.name)
    setTriggerType(t.trigger_type)
    const cfgInterval = t.trigger_config?.interval
    if (typeof cfgInterval === "string") setIntervalVal(cfgInterval)
    setScopeIds(t.scope_workbook_ids ?? [])
    setCondition(t.condition ?? "")
    setActions((t.actions ?? []).map((a) => actionToUI(a, allowLegacy)))
    setMaxSpend(
      t.max_spend_usd_per_day != null ? String(t.max_spend_usd_per_day) : "",
    )
    setMaxActions(
      t.max_actions_per_day != null ? String(t.max_actions_per_day) : "",
    )
    setStopOnError(t.stop_on_error)
    setSavedId(t.id)
    setHydrated(true)
  }, [editTriggerId, hydrated, existing.data, allowLegacy])

  // scoped columns from selected workbooks (for condition fields + re_enrich)
  const scopedColumns = useMemo(() => {
    const wbs = workbooks.data?.workbooks ?? []
    const seen = new Set<string>()
    const out: { id: string; name: string }[] = []
    for (const wb of wbs) {
      if (!scopeIds.includes(wb.id)) continue
      for (const col of wb.columns_config ?? []) {
        if (seen.has(col.id)) continue
        seen.add(col.id)
        out.push({ id: col.id, name: col.name })
      }
    }
    return out
  }, [workbooks.data, scopeIds])

  const cap = maxSpend.trim() === "" ? null : Number(maxSpend)
  const overCap =
    cap != null &&
    !Number.isNaN(cap) &&
    previewResult != null &&
    previewResult.projected_total_usd > cap

  const addActionTypes: ActionType[] = [
    ...DEFAULT_ACTION_TYPES,
    ...(allowLegacy ? LEGACY_ACTION_TYPES : []),
  ]

  // ── validation + serialization ────────────────────────────────────────────
  function buildRequest(): { body?: CreateTriggerRequest; error?: string } {
    if (!name.trim()) return { error: "Name is required." }
    if (triggerType === "on_schedule" && !interval)
      return { error: "Scheduled rules need an interval." }
    if (actions.length === 0) return { error: "Add at least one action." }
    const serialized: RuleAction[] = []
    for (const a of actions) {
      const { action, error } = uiToAction(a)
      if (error) return { error }
      if (action) serialized.push(action)
    }
    // re_enrich columns must be within scope
    const scopeColIds = new Set(scopedColumns.map((c) => c.id))
    for (const a of actions) {
      if (a.type === "re_enrich") {
        for (const cid of a.columnIds) {
          if (!scopeColIds.has(cid))
            return {
              error: "A re-enrich column is no longer in the scoped workbooks.",
            }
        }
      }
    }
    const body: CreateTriggerRequest = {
      name: name.trim(),
      trigger_type: triggerType,
      trigger_config: triggerType === "on_schedule" ? { interval } : {},
      condition: condition.trim(),
      actions: serialized,
      scope_workbook_ids: scopeIds,
      stop_on_error: stopOnError,
      max_spend_usd_per_day: cap != null && !Number.isNaN(cap) ? cap : null,
      max_actions_per_day: maxActions.trim() === "" ? null : Number(maxActions),
    }
    return { body }
  }

  /** Persist (create or patch); resolves to the trigger id, or null on error. */
  function persist(): Promise<string | null> {
    const { body, error } = buildRequest()
    if (error) {
      toast.error(error)
      return Promise.resolve(null)
    }
    setConditionError(null)
    return new Promise((resolve) => {
      const onErr = (err: unknown) => {
        if (
          err &&
          typeof err === "object" &&
          "status" in err &&
          (err as { status: number }).status === 422 &&
          /condition/i.test(apiErrorMessage(err))
        ) {
          setConditionError(apiErrorMessage(err))
        }
        resolve(null)
      }
      if (savedId) {
        patch.mutate(
          { id: savedId, body: body as CreateTriggerRequest },
          {
            onSuccess: (t) => {
              setSavedId((t as Trigger).id)
              resolve((t as Trigger).id)
            },
            onError: onErr,
          },
        )
      } else {
        create.mutate(body as CreateTriggerRequest, {
          onSuccess: (t) => {
            setSavedId((t as Trigger).id)
            resolve((t as Trigger).id)
          },
          onError: onErr,
        })
      }
    })
  }

  async function handleSave() {
    const id = await persist()
    if (id) {
      toast.success("Rule saved")
      onSaved(id)
    }
  }

  async function handlePreview() {
    const id = await persist()
    if (!id) return
    preview.mutate(
      { id, body: { limit: 25 } },
      { onSuccess: (res) => setPreviewResult(res as PreviewResult) },
    )
  }

  async function handleOpenRun() {
    // ensure saved + a fresh preview for cost display
    const id = await persist()
    if (!id) return
    preview.mutate(
      { id, body: { limit: 25 } },
      { onSuccess: (res) => setPreviewResult(res as PreviewResult) },
    )
    setRunOpen(true)
  }

  function doRun(dryRun: boolean) {
    if (!savedId) return
    run.mutate(
      { id: savedId, body: { dry_run: dryRun } },
      {
        onSuccess: (h) => {
          toast.success(dryRun ? "Dry run queued" : "Run queued")
          setRunOpen(false)
          onRan(savedId, (h as { fire_key: string }).fire_key)
        },
      },
    )
  }

  const saving = create.isPending || patch.isPending
  const loadingEdit = !!editTriggerId && existing.isLoading

  if (editTriggerId && isFeatureDisabledError(existing.error)) {
    return <FeatureDisabledFromError error={existing.error} feature={FEATURE} />
  }

  return (
    <div className="flex flex-col gap-4 p-4">
      <header className="flex items-center gap-2">
        <Button
          variant="ghost"
          size="icon"
          className="size-7"
          aria-label="Back"
          onClick={onCancel}
        >
          <ArrowLeft className="size-4" />
        </Button>
        <h1 className="text-base font-semibold">
          {editTriggerId ? "Edit rule" : "New rule"}
        </h1>
      </header>
      <Separator />

      {loadingEdit ? (
        <Loading rows={5} />
      ) : (
        <div className="flex max-w-3xl flex-col gap-5">
          {/* Name */}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="rule-name">Name</Label>
            <Input
              id="rule-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Re-enrich new rows"
              className="h-8 max-w-md text-sm"
            />
          </div>

          {/* Trigger */}
          <fieldset className="flex flex-col gap-1.5">
            <legend className="mb-1.5 text-sm font-medium">Trigger</legend>
            <div className="flex flex-wrap items-center gap-2">
              <Select
                value={triggerType}
                onValueChange={(v) =>
                  setTriggerType((v ?? "on_row_added") as TriggerType)
                }
              >
                <SelectTrigger className="h-8 w-[200px] text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(Object.keys(TRIGGER_TYPE_LABELS) as TriggerType[]).map(
                    (tt) => (
                      <SelectItem key={tt} value={tt}>
                        {TRIGGER_TYPE_LABELS[tt]}
                      </SelectItem>
                    ),
                  )}
                </SelectContent>
              </Select>
              {triggerType === "on_schedule" && (
                <Select
                  value={interval}
                  onValueChange={(v) => setIntervalVal(v ?? "daily")}
                >
                  <SelectTrigger className="h-8 w-[140px] text-sm">
                    <SelectValue placeholder="interval" />
                  </SelectTrigger>
                  <SelectContent>
                    {SCHEDULE_INTERVALS.map((iv) => (
                      <SelectItem key={iv} value={iv}>
                        {iv}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            </div>
            {triggerType === "on_signal" && (
              <p className="flex items-start gap-1 text-[11px] text-muted-foreground">
                <AlertTriangle className="mt-0.5 size-3 shrink-0" />
                On-signal rules need the Postgres lead store. If it is not
                enabled, saving returns a 409 and your form is kept so you can
                switch the trigger type.
              </p>
            )}
          </fieldset>

          {/* Scope */}
          <fieldset className="flex flex-col gap-1.5">
            <legend className="mb-1.5 text-sm font-medium">
              Scope workbooks
            </legend>
            {workbooks.isLoading ? (
              <Loading label="Loading workbooks…" />
            ) : (workbooks.data?.workbooks ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No workbooks yet. The rule will apply to all rows in scope.
              </p>
            ) : (
              <div className="flex flex-col gap-1.5">
                {(workbooks.data?.workbooks ?? []).map((wb) => {
                  const checked = scopeIds.includes(wb.id)
                  return (
                    <label
                      key={wb.id}
                      className="flex items-center gap-2 text-sm"
                    >
                      <Checkbox
                        checked={checked}
                        onCheckedChange={(c) =>
                          setScopeIds((prev) =>
                            c
                              ? [...prev, wb.id]
                              : prev.filter((x) => x !== wb.id),
                          )
                        }
                      />
                      <span className="truncate">{wb.name}</span>
                    </label>
                  )
                })}
              </div>
            )}
          </fieldset>

          {/* Condition */}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="rule-condition">Condition (optional)</Label>
            <div className="flex items-start gap-2">
              <Input
                id="rule-condition"
                value={condition}
                onChange={(e) => {
                  setCondition(e.target.value)
                  setConditionError(null)
                }}
                placeholder="{score} >= 80"
                className="h-8 flex-1 text-sm"
                aria-invalid={!!conditionError}
              />
              <ConditionFieldPicker
                fields={scopedColumns}
                onInsert={(token) =>
                  setCondition((c) => (c ? `${c} ${token}` : token))
                }
              />
            </div>
            <p className="text-[11px] text-muted-foreground">
              Operators: {CONDITION_OPERATORS}
            </p>
            {conditionError && (
              <p className="text-[11px] text-destructive">{conditionError}</p>
            )}
          </div>

          {/* Actions */}
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1.5 text-sm font-medium">
              Actions (run in order)
            </legend>
            {actions.length === 0 && (
              <p className="text-xs text-muted-foreground">
                No actions yet. Add at least one.
              </p>
            )}
            {actions.map((a, i) => (
              <ActionRow
                key={i}
                index={i}
                total={actions.length}
                label={
                  <span className="flex items-center gap-1.5">
                    {ACTION_TYPE_LABELS[a.type] ?? a.type}
                    {a.readOnly && (
                      <Badge variant="outline" className="text-[10px]">
                        legacy · read-only
                      </Badge>
                    )}
                  </span>
                }
                onMoveUp={() => setActions((prev) => move(prev, i, i - 1))}
                onMoveDown={() => setActions((prev) => move(prev, i, i + 1))}
                onRemove={() =>
                  setActions((prev) => prev.filter((_, j) => j !== i))
                }
              >
                <ActionConfigForm
                  action={a}
                  scopedColumns={scopedColumns}
                  onChange={(next) =>
                    setActions((prev) =>
                      prev.map((x, j) => (j === i ? next : x)),
                    )
                  }
                />
              </ActionRow>
            ))}
            <div className="flex items-center gap-2">
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="h-7 gap-1 text-xs"
                    />
                  }
                >
                  <Plus className="size-3" /> Add action
                </DropdownMenuTrigger>
                <DropdownMenuContent align="start">
                  {addActionTypes.map((at) => (
                    <DropdownMenuItem
                      key={at}
                      onSelect={() =>
                        setActions((prev) => [...prev, emptyAction(at)])
                      }
                    >
                      {ACTION_TYPE_LABELS[at]}
                    </DropdownMenuItem>
                  ))}
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
            {!allowLegacy && (
              <p className="text-[11px] text-muted-foreground">
                Email/Sequence actions are part of legacy outreach automation;
                enable <code>AUTOMATIONS_ALLOW_LEGACY_OUTREACH</code> to use
                them.
              </p>
            )}
          </fieldset>

          {/* Caps */}
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1.5 text-sm font-medium">Caps</legend>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="cap-spend">Max $/day</Label>
                <Input
                  id="cap-spend"
                  type="number"
                  min="0"
                  step="0.01"
                  value={maxSpend}
                  onChange={(e) => setMaxSpend(e.target.value)}
                  placeholder="10"
                  className="h-8 text-sm"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="cap-actions">Max actions/day</Label>
                <Input
                  id="cap-actions"
                  type="number"
                  min="0"
                  step="1"
                  value={maxActions}
                  onChange={(e) => setMaxActions(e.target.value)}
                  placeholder="500"
                  className="h-8 text-sm"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="cap-stop">Stop on error</Label>
                <div className="flex h-8 items-center">
                  <Switch
                    id="cap-stop"
                    checked={stopOnError}
                    onCheckedChange={(c) => setStopOnError(c)}
                  />
                </div>
              </div>
            </div>
          </fieldset>

          {/* Preview */}
          {previewResult && (
            <PreviewPanel result={previewResult} cap={cap} overCap={overCap} />
          )}

          {/* Footer */}
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="h-8 text-xs"
              disabled={preview.isPending || saving}
              onClick={handlePreview}
            >
              {preview.isPending ? "Previewing…" : "Dry-run preview"}
            </Button>
            <div className="flex-1" />
            <Button
              variant="ghost"
              size="sm"
              className="h-8 text-xs"
              onClick={onCancel}
            >
              Cancel
            </Button>
            <Gate need="admin">
              <Button
                size="sm"
                className="h-8 text-xs"
                disabled={saving}
                onClick={handleSave}
              >
                {saving ? "Saving…" : "Save"}
              </Button>
            </Gate>
            <Gate need="admin">
              <Button
                variant="secondary"
                size="sm"
                className="h-8 text-xs"
                disabled={saving}
                onClick={handleOpenRun}
              >
                Save &amp; Run…
              </Button>
            </Gate>
          </div>
        </div>
      )}

      <RunConfirmDialog
        open={runOpen}
        onOpenChange={setRunOpen}
        preview={previewResult}
        cap={cap}
        overCap={overCap}
        pending={run.isPending}
        canAdmin={canAdmin}
        onConfirm={doRun}
      />
    </div>
  )
}

// ── Per-action config sub-form ──────────────────────────────────────────────

function ActionConfigForm({
  action,
  scopedColumns,
  onChange,
}: {
  action: UIAction
  scopedColumns: { id: string; name: string }[]
  onChange: (next: UIAction) => void
}) {
  if (action.readOnly) {
    return (
      <p className="text-[11px] text-muted-foreground">
        Legacy action configured elsewhere. Enable legacy outreach to edit it.
      </p>
    )
  }

  if (action.type === "re_enrich") {
    return (
      <div className="flex flex-col gap-1.5">
        <span className="text-[11px] text-muted-foreground">
          Columns to enrich (from scoped workbooks)
        </span>
        {scopedColumns.length === 0 ? (
          <p className="text-[11px] text-muted-foreground">
            Select scope workbooks above to choose columns.
          </p>
        ) : (
          <div className="flex flex-wrap gap-x-4 gap-y-1.5">
            {scopedColumns.map((col) => {
              const checked = action.columnIds.includes(col.id)
              return (
                <label
                  key={col.id}
                  className="flex items-center gap-1.5 text-xs"
                >
                  <Checkbox
                    checked={checked}
                    onCheckedChange={(c) =>
                      onChange({
                        ...action,
                        columnIds: c
                          ? [...action.columnIds, col.id]
                          : action.columnIds.filter((x) => x !== col.id),
                      })
                    }
                  />
                  {col.name}
                </label>
              )
            })}
          </div>
        )}
      </div>
    )
  }

  if (action.type === "push_crm") {
    return (
      <div className="flex flex-col gap-2">
        <div className="flex items-center gap-2">
          <span className="text-[11px] text-muted-foreground">CRM</span>
          <Select
            value={action.crmType}
            onValueChange={(v) =>
              onChange({ ...action, crmType: v ?? "hubspot" })
            }
          >
            <SelectTrigger className="h-7 w-[150px] text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CRM_TYPES.map((c) => (
                <SelectItem key={c} value={c}>
                  {c}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <span className="text-[11px] text-muted-foreground">
          Field map (CRM field ← source column/token)
        </span>
        <div className="flex flex-col gap-1.5">
          {action.crmFields.map((row, ri) => (
            <div key={ri} className="flex items-center gap-1.5">
              <Input
                value={row.k}
                onChange={(e) =>
                  onChange({
                    ...action,
                    crmFields: action.crmFields.map((r, j) =>
                      j === ri ? { ...r, k: e.target.value } : r,
                    ),
                  })
                }
                placeholder="crm_field"
                className="h-7 flex-1 text-xs"
              />
              <span className="text-muted-foreground">←</span>
              <Input
                value={row.v}
                onChange={(e) =>
                  onChange({
                    ...action,
                    crmFields: action.crmFields.map((r, j) =>
                      j === ri ? { ...r, v: e.target.value } : r,
                    ),
                  })
                }
                placeholder="{email}"
                className="h-7 flex-1 text-xs"
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-7 text-destructive"
                aria-label="Remove field mapping"
                onClick={() =>
                  onChange({
                    ...action,
                    crmFields: action.crmFields.filter((_, j) => j !== ri),
                  })
                }
              >
                <Trash2 className="size-3.5" />
              </Button>
            </div>
          ))}
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 w-fit gap-1 text-xs"
            onClick={() =>
              onChange({
                ...action,
                crmFields: [...action.crmFields, { k: "", v: "" }],
              })
            }
          >
            <Plus className="size-3" /> Add mapping
          </Button>
        </div>
      </div>
    )
  }

  // webhook
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1.5">
        <Select
          value={action.webhookMethod}
          onValueChange={(v) =>
            onChange({ ...action, webhookMethod: v ?? "POST" })
          }
        >
          <SelectTrigger className="h-7 w-[90px] text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {WEBHOOK_METHODS.map((m) => (
              <SelectItem key={m} value={m}>
                {m}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          value={action.webhookUrl}
          onChange={(e) => onChange({ ...action, webhookUrl: e.target.value })}
          placeholder="https://example.com/hook"
          className="h-7 min-w-[200px] flex-1 text-xs"
        />
      </div>
      <Textarea
        value={action.webhookBody}
        onChange={(e) => onChange({ ...action, webhookBody: e.target.value })}
        placeholder='Optional JSON body, e.g. {"row": "{row_id}"}'
        className="min-h-16 font-mono text-xs"
      />
      <div className="flex items-center gap-1.5">
        <span className="text-[11px] text-muted-foreground">
          Header secret ref
        </span>
        <Input
          value={action.webhookSecretRef}
          onChange={(e) =>
            onChange({ ...action, webhookSecretRef: e.target.value })
          }
          placeholder="my_webhook_secret"
          className="h-7 max-w-[220px] text-xs"
        />
      </div>
      <p className="text-[11px] text-muted-foreground">
        Reference name only — the secret value is stored server-side, never here.
      </p>
    </div>
  )
}

// ── Preview panel ───────────────────────────────────────────────────────────

function PreviewPanel({
  result,
  cap,
  overCap,
}: {
  result: PreviewResult
  cap: number | null
  overCap: boolean
}) {
  return (
    <Card className="flex flex-col gap-2 p-3">
      <div className="flex items-center justify-between text-sm">
        <span className="font-medium">Dry-run preview</span>
        <span className="text-xs text-muted-foreground">
          {result.matched} matched
        </span>
      </div>
      <div className="flex items-baseline gap-2">
        <span className="text-xs text-muted-foreground">Projected total</span>
        <span
          className={`text-base font-semibold ${
            overCap ? "text-destructive" : ""
          }`}
        >
          {usd(result.projected_total_usd)}
        </span>
        {overCap && cap != null && (
          <Badge variant="destructive" className="text-[10px]">
            over daily cap ({usd(cap)})
          </Badge>
        )}
      </div>
      {result.sample.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="text-xs">Row</TableHead>
                <TableHead className="text-xs">Pass</TableHead>
                <TableHead className="text-xs">Action</TableHead>
                <TableHead className="text-right text-xs">
                  Would charge
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {result.sample.slice(0, 10).flatMap((row) =>
                row.actions.length === 0
                  ? [
                      <TableRow key={row.row_id}>
                        <TableCell className="text-xs">{row.row_id}</TableCell>
                        <TableCell className="text-xs">
                          {row.condition_pass ? "✓" : "✗"}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          —
                        </TableCell>
                        <TableCell className="text-right text-xs">—</TableCell>
                      </TableRow>,
                    ]
                  : row.actions.map((a, ai) => (
                      <TableRow key={`${row.row_id}-${ai}`}>
                        <TableCell className="text-xs">
                          {ai === 0 ? row.row_id : ""}
                        </TableCell>
                        <TableCell className="text-xs">
                          {ai === 0 ? (row.condition_pass ? "✓" : "✗") : ""}
                        </TableCell>
                        <TableCell className="text-xs">
                          {ACTION_TYPE_LABELS[a.type] ?? a.type}
                        </TableCell>
                        <TableCell className="text-right text-xs">
                          {usd(a.would_charge_usd)}
                        </TableCell>
                      </TableRow>
                    )),
              )}
            </TableBody>
          </Table>
        </div>
      )}
    </Card>
  )
}

// ── Run confirm dialog ──────────────────────────────────────────────────────

function RunConfirmDialog({
  open,
  onOpenChange,
  preview,
  cap,
  overCap,
  pending,
  canAdmin,
  onConfirm,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  preview: PreviewResult | null
  cap: number | null
  overCap: boolean
  pending: boolean
  canAdmin: boolean
  onConfirm: (dryRun: boolean) => void
}) {
  const [dryRun, setDryRun] = useState(true)
  const [understood, setUnderstood] = useState(false)

  // reset when (re)opened
  useEffect(() => {
    if (open) {
      setDryRun(true)
      setUnderstood(false)
    }
  }, [open])

  const needAck = !dryRun && overCap
  const confirmDisabled = !canAdmin || (needAck && !understood)

  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Run rule"
      description="The rule has been saved. Choose how to run it."
      confirmLabel={dryRun ? "Run dry run" : "Run for real"}
      destructive={!dryRun}
      confirmDisabled={confirmDisabled}
      pending={pending}
      onConfirm={() => onConfirm(dryRun)}
    >
      <div className="flex flex-col gap-3 text-sm">
        {preview ? (
          <div className="flex items-baseline gap-2">
            <span className="text-xs text-muted-foreground">
              Projected total
            </span>
            <span
              className={`font-semibold ${overCap ? "text-destructive" : ""}`}
            >
              {usd(preview.projected_total_usd)}
            </span>
            <span className="text-xs text-muted-foreground">
              · {preview.matched} matched
            </span>
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">
            Run a dry-run preview first for a cost estimate.
          </p>
        )}
        <label className="flex items-center gap-2 text-sm">
          <Switch checked={dryRun} onCheckedChange={(c) => setDryRun(c)} />
          Dry run (no charges, no side effects)
        </label>
        {needAck && cap != null && preview && (
          <label className="flex items-start gap-2 rounded-md border border-destructive/40 p-2 text-xs">
            <Checkbox
              checked={understood}
              onCheckedChange={(c) => setUnderstood(!!c)}
            />
            <span>
              Projected {usd(preview.projected_total_usd)} exceeds the daily cap
              of {usd(cap)}. I understand and want to run for real.
            </span>
          </label>
        )}
        {!canAdmin && (
          <p className="text-xs text-destructive">
            Running a rule requires the admin role for this workspace.
          </p>
        )}
      </div>
    </ConfirmDialog>
  )
}

// ════════════════════════════════ Detail ═══════════════════════════════════

function TriggerDetail({
  triggerId,
  pendingFireKey,
  onBack,
  onEdit,
  onOpenRun,
}: {
  triggerId: string
  pendingFireKey?: string
  onBack: () => void
  onEdit: () => void
  onOpenRun: (runId: string) => void
}) {
  const trigger = useTrigger(triggerId)
  const runs = useRuns(triggerId)
  const pause = usePauseTrigger()
  const resume = useResumeTrigger()
  const { allowed: canAdmin } = useCanRole("admin")
  const [autoOpened, setAutoOpened] = useState(false)

  // Save & Run → match the new TriggerRun by fire_key, then open it.
  useEffect(() => {
    if (!pendingFireKey || autoOpened || !runs.data) return
    const match = runs.data.find((r) => r.fire_key === pendingFireKey)
    if (match) {
      setAutoOpened(true)
      onOpenRun(match.id)
    }
  }, [pendingFireKey, autoOpened, runs.data, onOpenRun])

  if (isFeatureDisabledError(trigger.error)) {
    return <FeatureDisabledFromError error={trigger.error} feature={FEATURE} />
  }

  const t = trigger.data

  // Synthetic pending row while the run materializes.
  const runList: TriggerRun[] = runs.data ?? []
  const showPending =
    !!pendingFireKey && !runList.some((r) => r.fire_key === pendingFireKey)

  return (
    <div className="flex flex-col gap-4 p-4">
      <header className="flex items-center gap-2">
        <Button
          variant="ghost"
          size="icon"
          className="size-7"
          aria-label="Back"
          onClick={onBack}
        >
          <ArrowLeft className="size-4" />
        </Button>
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-base font-semibold">
            {t?.name ?? "Rule"}
          </h1>
          {t && (
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <StatusDot status={t.enabled ? "enabled" : "disabled"} />
              <span>
                {TRIGGER_TYPE_LABELS[t.trigger_type] ?? t.trigger_type}
              </span>
            </div>
          )}
        </div>
        {t && (
          <div className="flex items-center gap-1.5">
            <Gate need="admin">
              <Button
                variant="outline"
                size="sm"
                className="h-7 text-xs"
                onClick={onEdit}
              >
                Edit
              </Button>
            </Gate>
            {t.enabled ? (
              <Gate need="admin">
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 gap-1 text-xs"
                  disabled={pause.isPending}
                  onClick={() => pause.mutate(triggerId)}
                >
                  <Pause className="size-3" /> Pause
                </Button>
              </Gate>
            ) : (
              <Gate need="admin">
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 gap-1 text-xs"
                  disabled={resume.isPending}
                  onClick={() => resume.mutate(triggerId)}
                >
                  <Play className="size-3" /> Enable
                </Button>
              </Gate>
            )}
          </div>
        )}
      </header>
      <Separator />

      {trigger.isLoading ? (
        <Loading rows={4} />
      ) : trigger.isError ? (
        <ErrorState error={trigger.error} onRetry={() => trigger.refetch()} />
      ) : !t ? null : (
        <>
          {/* Rule summary */}
          <Card className="flex flex-col gap-2 p-3 text-xs">
            <div className="flex flex-wrap gap-x-6 gap-y-1">
              <span>
                <span className="text-muted-foreground">Condition: </span>
                {t.condition || "—"}
              </span>
              <span>
                <span className="text-muted-foreground">Scope: </span>
                {t.scope_workbook_ids.length} workbook(s)
              </span>
              <span>
                <span className="text-muted-foreground">Max $/day: </span>
                {t.max_spend_usd_per_day != null
                  ? usd(t.max_spend_usd_per_day)
                  : "—"}
              </span>
              <span>
                <span className="text-muted-foreground">Stop on error: </span>
                {t.stop_on_error ? "yes" : "no"}
              </span>
            </div>
            <div className="flex flex-wrap gap-1.5">
              {t.actions.map((a, i) => (
                <Badge key={i} variant="secondary" className="text-[10px]">
                  {i + 1}. {ACTION_TYPE_LABELS[a.type] ?? a.type}
                </Badge>
              ))}
            </div>
          </Card>

          {/* Recent runs */}
          <div className="flex flex-col gap-2">
            <h2 className="text-sm font-medium">Recent runs</h2>
            {runs.isLoading ? (
              <Loading rows={3} />
            ) : runList.length === 0 && !showPending ? (
              <Empty
                title="No runs yet"
                description={
                  canAdmin
                    ? "Edit the rule and use Save & Run to test it."
                    : "Runs will appear here once an admin runs the rule."
                }
              />
            ) : (
              <div className="flex flex-col gap-1.5">
                {showPending && (
                  <div className="flex items-center gap-3 rounded-lg border border-dashed p-3 text-sm text-muted-foreground">
                    <StatusDot status="queued" />
                    <span className="flex-1">Starting…</span>
                    <span className="text-xs">just now</span>
                  </div>
                )}
                {runList.map((r) => (
                  <button
                    key={r.id}
                    type="button"
                    className="flex items-center gap-3 rounded-lg border p-3 text-left text-sm transition-colors hover:bg-muted/50"
                    onClick={() => onOpenRun(r.id)}
                  >
                    <StatusDot status={r.status} />
                    <span className="flex-1 truncate">
                      {r.fire_source}
                      {isRunActive(r.status) && (
                        <span className="ml-2 text-xs text-muted-foreground">
                          running…
                        </span>
                      )}
                    </span>
                    <span className="hidden text-xs text-muted-foreground sm:inline">
                      {r.matched_rows} matched · {usd(r.total_charged_usd)}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {relativeTime(r.started_at ?? r.finished_at)}
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}

// ════════════════════════════════ Run view ═════════════════════════════════

function RunDetail({ runId, onBack }: { runId: string; onBack: () => void }) {
  const run = useRun(runId)

  if (isFeatureDisabledError(run.error)) {
    return <FeatureDisabledFromError error={run.error} feature={FEATURE} />
  }

  const r = run.data

  return (
    <div className="flex flex-col gap-4 p-4">
      <header className="flex items-center gap-2">
        <Button
          variant="ghost"
          size="icon"
          className="size-7"
          aria-label="Back"
          onClick={onBack}
        >
          <ArrowLeft className="size-4" />
        </Button>
        <h1 className="text-base font-semibold">Run detail</h1>
        {r && (
          <div className="ml-1">
            <StatusDot status={r.status} />
          </div>
        )}
      </header>
      <Separator />

      {run.isLoading ? (
        <Loading rows={4} />
      ) : run.isError ? (
        <ErrorState error={run.error} onRetry={() => run.refetch()} />
      ) : !r ? null : (
        <>
          {/* Counts */}
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
            <Stat label="Matched" value={String(r.matched_rows)} />
            <Stat label="Attempted" value={String(r.actions_attempted)} />
            <Stat label="Succeeded" value={String(r.actions_succeeded)} />
            <Stat label="Skipped" value={String(r.actions_skipped)} />
            <Stat label="Failed" value={String(r.actions_failed)} />
          </div>
          <div className="flex items-center gap-2 text-sm">
            <span className="text-muted-foreground">Total charged</span>
            <span className="font-semibold">{usd(r.total_charged_usd)}</span>
            {isRunActive(r.status) && (
              <Badge variant="secondary" className="text-[10px]">
                live · refreshing
              </Badge>
            )}
          </div>
          {r.error && (
            <p className="rounded-md border border-destructive/40 p-2 text-xs text-destructive">
              {r.error}
            </p>
          )}

          {/* Action results */}
          <div className="flex flex-col gap-2">
            <h2 className="text-sm font-medium">Action results</h2>
            {r.action_results.length === 0 ? (
              <Empty
                title="No action results"
                description={
                  isRunActive(r.status)
                    ? "Results will appear as the run progresses."
                    : "This run produced no action results."
                }
              />
            ) : (
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="text-xs">#</TableHead>
                      <TableHead className="text-xs">Action</TableHead>
                      <TableHead className="text-xs">Status</TableHead>
                      <TableHead className="text-right text-xs">Charge</TableHead>
                      <TableHead className="text-xs">Summary</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {r.action_results.map((ar) => (
                      <TableRow key={ar.id}>
                        <TableCell className="text-xs">
                          {ar.action_index + 1}
                        </TableCell>
                        <TableCell className="text-xs">
                          {ACTION_TYPE_LABELS[ar.action_type] ?? ar.action_type}
                        </TableCell>
                        <TableCell className="text-xs">
                          <StatusDot status={ar.status} />
                        </TableCell>
                        <TableCell className="text-right text-xs">
                          {usd(ar.charged_usd)}
                        </TableCell>
                        <TableCell className="max-w-[280px] truncate text-xs text-muted-foreground">
                          {ar.error ||
                            ar.skip_reason ||
                            ar.result_summary ||
                            "—"}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col rounded-lg border p-2">
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span className="text-lg font-semibold">{value}</span>
    </div>
  )
}
