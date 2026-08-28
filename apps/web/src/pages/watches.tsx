// Intent Watches page (spec §1.3).
//
// Replaces the scaffold placeholder with the full slice:
//   * 404 (INTENT_POLLER_ENABLED off)            → FeatureDisabled variant="flag-off"
//   * 409 (intent_poller_requires_pg_lead_store) → FeatureDisabled variant="dependency"
//   * WatchList   — next-poll / disabled / fail-count + enable·pause·delete (ConfirmDialog on delete)
//   * WatchBuilder — kind→target→interval→signal_types (per-kind set) + optional lead_id + webhook rule
//   * WatchDetail  — Poll now (202 already_queued / 409 disabled / two distinct 429) + SignalFeed
//
// All data access goes through the scaffold hooks (useWatches / useWatch /
// useWatchSignals + mutations). Watch mutations require editor/admin → wrapped
// in <Gate need="editor"> for proactive disable + tooltip; the reactive 403
// handler in the mutation hooks remains the backstop. Reads are any member.

import { useMemo, useState } from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import {
  ArrowLeft,
  ChevronRight,
  MoreHorizontal,
  Pause,
  Play,
  Plus,
  Radar,
  RefreshCw,
  Trash2,
  TriangleAlert,
} from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Checkbox } from "@/components/ui/checkbox"
import { Switch } from "@/components/ui/switch"
import { Separator } from "@/components/ui/separator"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

import { ConfirmDialog } from "@/components/confirm-dialog"
import { Empty, ErrorState, Loading } from "@/components/states"
import {
  FeatureDisabled,
  FeatureDisabledFromError,
  isFeatureDisabledError,
} from "@/components/feature-disabled"
import { SignalFeed } from "@/components/signal-feed"
import { StatusDot } from "@/components/status-dot"
import { Gate, useCanRole } from "@/components/gate"

import {
  apiErrorMessage,
  useCreateWatch,
  useDeleteWatch,
  useFlags,
  usePatchWatch,
  usePollWatch,
  useWatch,
  useWatchSignals,
  useWatches,
} from "@/lib/automation-hooks"
import {
  WATCH_KIND_SIGNAL_TYPES,
  type ApiError,
  type Watch,
  type WatchCreate,
  type WatchInterval,
  type WatchKind,
} from "@/lib/api"

const FEATURE = "Intent Watches"

const KIND_LABELS: Record<WatchKind, string> = {
  funding: "Funding",
  hiring: "Hiring",
  feed: "Feed",
  company: "Company",
}

const KINDS: WatchKind[] = ["funding", "hiring", "feed", "company"]
const INTERVALS: WatchInterval[] = ["daily", "hourly", "weekly"]

const SIGNAL_TYPE_LABELS: Record<string, string> = {
  company_funded: "Company funded",
  executive_hired: "Executive hired",
  hiring_surge: "Hiring surge",
  new_tech_adopted: "New tech adopted",
  news: "News",
}

function signalTypeLabel(t: string): string {
  return SIGNAL_TYPE_LABELS[t] ?? t
}

/** Relative "next poll" / "last polled" helper for ISO timestamps. */
function formatRelative(iso: string | null): string | null {
  if (!iso) return null
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return null
  const diff = t - Date.now()
  const abs = Math.abs(diff)
  const mins = Math.round(abs / 60000)
  const hours = Math.round(abs / 3600000)
  const days = Math.round(abs / 86400000)
  let rel: string
  if (mins < 60) rel = `${mins}m`
  else if (hours < 24) rel = `${hours}h`
  else rel = `${days}d`
  return diff >= 0 ? `next ${rel}` : `${rel} ago`
}

// ════════════════════════════════ Page ═════════════════════════════════════

type View = { name: "list" } | { name: "create" } | { name: "detail"; id: string }

export default function WatchesPage() {
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const initialTarget = searchParams.get("target") || ""
  const [view, setView] = useState<View>(() =>
    searchParams.get("create") === "1" ? { name: "create" } : { name: "list" },
  )
  const flags = useFlags()
  const watches = useWatches(
    undefined,
    flags.data?.intent_poller_enabled === true && flags.data.pg_lead_store,
  )

  // 404 (flag off) → flag-off screen; 409 (PG dep) → dependency screen.
  if (isFeatureDisabledError(watches.error)) {
    return <FeatureDisabledFromError error={watches.error} feature={FEATURE} />
  }
  // Proactive flag checks (avoid a flash before the query resolves).
  if (flags.data && !flags.data.intent_poller_enabled) {
    return <FeatureDisabled variant="flag-off" feature={FEATURE} />
  }
  if (flags.data && !flags.data.pg_lead_store) {
    return <FeatureDisabled variant="dependency" feature={FEATURE} />
  }

  if (view.name === "create") {
    return (
      <WatchBuilder
        initialTarget={initialTarget}
        onCancel={() => {
          setView({ name: "list" })
          navigate("/watches", { replace: true })
        }}
        onCreated={(id) => setView({ name: "detail", id })}
      />
    )
  }
  if (view.name === "detail") {
    return (
      <WatchDetail id={view.id} onBack={() => setView({ name: "list" })} />
    )
  }

  return (
    <WatchListView
      query={watches}
      onCreate={() => setView({ name: "create" })}
      onOpen={(id) => setView({ name: "detail", id })}
    />
  )
}

// ════════════════════════════════ List ═════════════════════════════════════

function PageHeader({
  subtitle,
  action,
}: {
  subtitle: string
  action?: React.ReactNode
}) {
  return (
    <div className="flex items-start justify-between gap-3">
      <div>
        <h2 className="flex items-center gap-2 text-base font-semibold">
          <Radar className="size-4" />
          {FEATURE}
        </h2>
        <p className="mt-0.5 text-xs text-muted-foreground">{subtitle}</p>
      </div>
      {action}
    </div>
  )
}

function WatchListView({
  query,
  onCreate,
  onOpen,
}: {
  query: ReturnType<typeof useWatches>
  onCreate: () => void
  onOpen: (id: string) => void
}) {
  const { allowed: canEdit } = useCanRole("editor")
  const watches = query.data?.watches ?? []

  const newButton = (
    <Gate need="editor">
      <Button size="sm" className="h-7 gap-1 text-xs" onClick={onCreate}>
        <Plus className="size-3" /> New Watch
      </Button>
    </Gate>
  )

  return (
    <div className="flex flex-col gap-4 p-4">
      <PageHeader
        subtitle="Poll funding, hiring, news, and company signals on a schedule."
        action={newButton}
      />
      {!canEdit && (
        <p className="text-[11px] text-muted-foreground">
          You can view watches and their signals; creating, polling, and editing
          require the editor or admin role.
        </p>
      )}
      <Separator />

      {query.isLoading ? (
        <Loading rows={4} />
      ) : query.isError ? (
        <ErrorState error={query.error} onRetry={() => query.refetch()} />
      ) : watches.length === 0 ? (
        <Empty
          icon={Radar}
          title="No watches yet"
          description="Create a watch to start polling for buying signals on a company or feed."
          action={newButton}
        />
      ) : (
        <div className="flex flex-col gap-1.5">
          {watches.map((w) => (
            <WatchRow key={w.id} watch={w} onOpen={onOpen} />
          ))}
        </div>
      )}
    </div>
  )
}

function WatchRow({
  watch,
  onOpen,
}: {
  watch: Watch
  onOpen: (id: string) => void
}) {
  const patch = usePatchWatch()
  const del = useDeleteWatch()
  const editAllowed = useCanRole("editor").allowed
  const [confirmOpen, setConfirmOpen] = useState(false)
  const nextPoll = formatRelative(watch.next_poll_at)

  const toggleEnabled = () =>
    patch.mutate({ id: watch.id, body: { enabled: !watch.enabled } })

  return (
    <div
      role="button"
      tabIndex={0}
      onClick={() => onOpen(watch.id)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault()
          onOpen(watch.id)
        }
      }}
      className="group flex cursor-pointer items-center gap-3 rounded-lg border px-3 py-2.5 text-xs transition-colors hover:bg-muted/50"
    >
      <StatusDot status={watch.enabled ? "enabled" : "disabled"} />
      <span className="min-w-0 flex-1 truncate font-medium">{watch.target}</span>
      <Badge variant="secondary" className="shrink-0 text-[10px]">
        {KIND_LABELS[watch.kind]}
      </Badge>
      <span className="shrink-0 text-[10px] text-muted-foreground">
        {watch.interval}
      </span>
      {watch.enabled && nextPoll && (
        <span className="shrink-0 text-[10px] text-muted-foreground/70">
          {nextPoll}
        </span>
      )}
      {watch.consecutive_failures > 0 && (
        <span
          className="flex shrink-0 items-center gap-1 text-[10px] text-amber-500"
          title={watch.last_error ?? "Recent poll failures"}
        >
          <TriangleAlert className="size-3" />
          {watch.consecutive_failures} fail
          {watch.consecutive_failures === 1 ? "" : "s"}
        </span>
      )}

      <DropdownMenu>
        <DropdownMenuTrigger onClick={(e) => e.stopPropagation()}>
          <span className="rounded p-1 opacity-0 transition-colors hover:bg-muted group-hover:opacity-100">
            <MoreHorizontal className="size-4" />
          </span>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem
            onClick={(e) => {
              e.stopPropagation()
              onOpen(watch.id)
            }}
          >
            View details
          </DropdownMenuItem>
          <DropdownMenuItem
            disabled={!editAllowed || patch.isPending}
            onClick={(e) => {
              e.stopPropagation()
              toggleEnabled()
            }}
          >
            {watch.enabled ? (
              <>
                <Pause className="size-3.5" /> Pause
              </>
            ) : (
              <>
                <Play className="size-3.5" /> Enable
              </>
            )}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            className="text-destructive"
            disabled={!editAllowed}
            onClick={(e) => {
              e.stopPropagation()
              setConfirmOpen(true)
            }}
          >
            <Trash2 className="size-3.5" /> Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <ChevronRight className="size-4 shrink-0 text-muted-foreground/30" />

      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title="Delete watch?"
        description={`This permanently deletes the watch on “${watch.target}”. Its detected signals are kept.`}
        confirmLabel="Delete"
        destructive
        pending={del.isPending}
        onConfirm={() =>
          del.mutate(watch.id, {
            onSuccess: () => {
              toast.success("Watch deleted")
              setConfirmOpen(false)
            },
          })
        }
      />
    </div>
  )
}

// ═══════════════════════════════ Builder ═══════════════════════════════════

function WatchBuilder({
  initialTarget,
  onCancel,
  onCreated,
}: {
  initialTarget?: string
  onCancel: () => void
  onCreated: (id: string) => void
}) {
  const create = useCreateWatch()
  const { allowed: canEdit } = useCanRole("editor")

  const [kind, setKind] = useState<WatchKind>(initialTarget ? "company" : "funding")
  const [target, setTarget] = useState(initialTarget || "")
  const [interval, setInterval] = useState<WatchInterval>("daily")
  const [leadId, setLeadId] = useState("")
  const [createWebhook, setCreateWebhook] = useState(false)
  const [webhookUrl, setWebhookUrl] = useState("")
  const [webhookErr, setWebhookErr] = useState<string | null>(null)

  // signal_types default to all-checked for the kind (mirrors backend default).
  const allowed = WATCH_KIND_SIGNAL_TYPES[kind]
  const [selectedTypes, setSelectedTypes] = useState<string[]>(allowed)

  // When kind changes, reset the allowed set to all-checked.
  const onKindChange = (next: WatchKind) => {
    setKind(next)
    setSelectedTypes(WATCH_KIND_SIGNAL_TYPES[next])
  }

  const toggleType = (t: string, checked: boolean) => {
    setSelectedTypes((prev) =>
      checked ? [...new Set([...prev, t])] : prev.filter((x) => x !== t),
    )
  }

  const targetValid = target.trim().length > 0
  const leadIdValid = leadId.trim() === "" || /^\d+$/.test(leadId.trim())
  const webhookUrlValid =
    !createWebhook ||
    /^https?:\/\/[^\s@]+$/.test(webhookUrl.trim()) // http/https, no embedded credentials (@)
  const hasTypes = selectedTypes.length > 0

  const canSubmit =
    targetValid && hasTypes && leadIdValid && webhookUrlValid && !create.isPending

  const submit = () => {
    setWebhookErr(null)
    if (!canSubmit) return
    const body: WatchCreate = {
      kind,
      target: target.trim(),
      interval,
      signal_types: selectedTypes,
    }
    if (leadId.trim()) body.lead_id = Number(leadId.trim())
    if (createWebhook) {
      body.create_webhook_rule = true
      body.webhook_url = webhookUrl.trim()
    }
    create.mutate(body, {
      onSuccess: (data) => {
        toast.success("Watch created")
        onCreated((data as Watch).id)
      },
      onError: (err) => {
        const e = err as ApiError
        // 409 automations_disabled_cannot_create_webhook_rule → inline note.
        if (
          e?.status === 409 &&
          /automations_disabled_cannot_create_webhook_rule/i.test(e.detail)
        ) {
          setWebhookErr("Webhook rules need Automations enabled.")
        } else if (e?.status === 422 && /webhook|http/i.test(e.detail ?? "")) {
          setWebhookErr(apiErrorMessage(e))
        }
      },
    })
  }

  return (
    <div className="flex flex-col gap-4 p-4">
      <div className="flex items-center gap-2">
        <Button
          variant="ghost"
          size="sm"
          className="h-7 gap-1 px-2 text-xs"
          onClick={onCancel}
        >
          <ArrowLeft className="size-3.5" /> Back
        </Button>
        <h2 className="text-base font-semibold">New Watch</h2>
      </div>
      <Separator />

      <div className="flex max-w-2xl flex-col gap-4">
        {/* Kind */}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="watch-kind">Kind</Label>
            <Select value={kind} onValueChange={(v) => v && onKindChange(v as WatchKind)}>
              <SelectTrigger id="watch-kind" className="h-9 text-xs">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {KINDS.map((k) => (
                  <SelectItem key={k} value={k} className="text-xs">
                    {KIND_LABELS[k]}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {/* Interval */}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="watch-interval">Interval</Label>
            <Select
              value={interval}
              onValueChange={(v) => v && setInterval(v as WatchInterval)}
            >
              <SelectTrigger id="watch-interval" className="h-9 text-xs">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {INTERVALS.map((i) => (
                  <SelectItem key={i} value={i} className="text-xs">
                    {i.charAt(0).toUpperCase() + i.slice(1)}
                    {i === "daily" ? " (default)" : ""}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>

        {/* Target */}
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="watch-target">Target</Label>
          <Input
            id="watch-target"
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            placeholder={
              kind === "feed"
                ? "Feed URL or keyword"
                : "Company name or domain"
            }
            className="h-9 text-xs"
          />
        </div>

        {/* signal_types — fieldset/legend for AT grouping */}
        <fieldset className="flex flex-col gap-2 rounded-md border p-3">
          <legend className="px-1 text-xs font-medium">Signal types</legend>
          <p className="text-[11px] text-muted-foreground">
            Limited to the types this watch kind can emit.
          </p>
          <div className="flex flex-wrap gap-3">
            {allowed.map((t) => {
              const id = `sigtype-${t}`
              return (
                <label
                  key={t}
                  htmlFor={id}
                  className="flex cursor-pointer items-center gap-1.5 text-xs"
                >
                  <Checkbox
                    id={id}
                    checked={selectedTypes.includes(t)}
                    onCheckedChange={(c) => toggleType(t, c === true)}
                  />
                  {signalTypeLabel(t)}
                </label>
              )
            })}
          </div>
          {!hasTypes && (
            <p className="text-[11px] text-destructive">
              Select at least one signal type.
            </p>
          )}
        </fieldset>

        {/* Optional lead_id */}
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="watch-lead">
            Linked lead ID{" "}
            <span className="font-normal text-muted-foreground">(optional)</span>
          </Label>
          <Input
            id="watch-lead"
            value={leadId}
            onChange={(e) => setLeadId(e.target.value)}
            placeholder="e.g. 1024"
            inputMode="numeric"
            className="h-9 max-w-[12rem] text-xs"
          />
          {!leadIdValid && (
            <p className="text-[11px] text-destructive">
              Lead ID must be a number.
            </p>
          )}
        </div>

        {/* Webhook rule */}
        <div className="flex flex-col gap-2 rounded-md border p-3">
          <div className="flex items-center justify-between gap-3">
            <div className="flex flex-col">
              <Label htmlFor="watch-webhook-switch" className="text-xs">
                Create webhook rule
              </Label>
              <span className="text-[11px] text-muted-foreground">
                Forward detected signals to a webhook (requires Automations).
              </span>
            </div>
            <Switch
              id="watch-webhook-switch"
              checked={createWebhook}
              onCheckedChange={(c) => {
                setCreateWebhook(c)
                setWebhookErr(null)
              }}
            />
          </div>
          {createWebhook && (
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="watch-webhook-url">Webhook URL</Label>
              <Input
                id="watch-webhook-url"
                value={webhookUrl}
                onChange={(e) => {
                  setWebhookUrl(e.target.value)
                  setWebhookErr(null)
                }}
                placeholder="https://example.com/hooks/opengtm"
                className="h-9 text-xs"
              />
              {!webhookUrlValid && webhookUrl.trim() !== "" && (
                <p className="text-[11px] text-destructive">
                  Enter a valid http(s) URL without embedded credentials.
                </p>
              )}
              {webhookErr && (
                <p className="text-[11px] text-destructive">{webhookErr}</p>
              )}
            </div>
          )}
        </div>

        {!canEdit && (
          <p className="text-[11px] text-amber-500">
            Creating a watch requires the editor or admin role for this workspace.
          </p>
        )}

        <div className="flex items-center gap-2">
          <Gate need="editor">
            <Button
              size="sm"
              className="h-8 text-xs"
              disabled={!canSubmit}
              onClick={submit}
            >
              {create.isPending ? "Creating…" : "Create watch"}
            </Button>
          </Gate>
          <Button
            variant="outline"
            size="sm"
            className="h-8 text-xs"
            onClick={onCancel}
          >
            Cancel
          </Button>
        </div>
      </div>
    </div>
  )
}

// ═══════════════════════════════ Detail ════════════════════════════════════

function WatchDetail({ id, onBack }: { id: string; onBack: () => void }) {
  const watchQ = useWatch(id)
  const signalsQ = useWatchSignals(id)
  const patch = usePatchWatch()
  const del = useDeleteWatch()
  const poll = usePollWatch()
  const [confirmOpen, setConfirmOpen] = useState(false)

  const watch = watchQ.data

  const pollNow = () => {
    if (!watch) return
    poll.mutate(id, {
      onSuccess: (data) => {
        const res = data as { status: string }
        if (res.status === "already_queued") {
          toast.info("Poll already queued — results will appear shortly.")
        } else {
          toast.success("Poll queued — results will appear shortly.")
        }
      },
      onError: (err) => {
        const e = err as ApiError
        if (e?.status === 409 && /watch disabled/i.test(e.detail ?? "")) {
          toast.error("Watch is disabled — enable it before polling.")
        } else if (e?.status === 429) {
          if (/quota/i.test(e.detail ?? "")) {
            toast.error("Daily poll quota reached — resets tomorrow.")
          } else {
            toast.error("Rate limited — try again shortly.")
          }
        }
        // other statuses already surfaced by the shared onError toast
      },
    })
  }

  const toggleEnabled = () => {
    if (!watch) return
    patch.mutate({ id, body: { enabled: !watch.enabled } })
  }

  const signals = signalsQ.data?.signals ?? []
  const editAllowed = useCanRole("editor").allowed

  return (
    <div className="flex flex-col gap-4 p-4">
      <div className="flex items-center gap-2">
        <Button
          variant="ghost"
          size="sm"
          className="h-7 gap-1 px-2 text-xs"
          onClick={onBack}
        >
          <ArrowLeft className="size-3.5" /> Back
        </Button>
      </div>

      {watchQ.isLoading ? (
        <Loading rows={3} />
      ) : watchQ.isError ? (
        <ErrorState error={watchQ.error} onRetry={() => watchQ.refetch()} />
      ) : !watch ? (
        <Empty title="Watch not found" />
      ) : (
        <>
          {/* Header */}
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex flex-col gap-1">
              <div className="flex items-center gap-2">
                <h2 className="text-base font-semibold">{watch.target}</h2>
                <Badge variant="secondary" className="text-[10px]">
                  {KIND_LABELS[watch.kind]}
                </Badge>
                <StatusDot status={watch.enabled ? "enabled" : "disabled"} />
              </div>
              <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground">
                <span>{watch.interval}</span>
                {watch.enabled && formatRelative(watch.next_poll_at) && (
                  <span>{formatRelative(watch.next_poll_at)}</span>
                )}
                {watch.last_polled_at && (
                  <span>
                    last polled {formatRelative(watch.last_polled_at)}
                  </span>
                )}
                {watch.consecutive_failures > 0 && (
                  <span className="flex items-center gap-1 text-amber-500">
                    <TriangleAlert className="size-3" />
                    {watch.consecutive_failures} consecutive fail
                    {watch.consecutive_failures === 1 ? "" : "s"}
                  </span>
                )}
              </div>
            </div>

            <div className="flex items-center gap-2">
              <Gate need="editor">
                <Button
                  size="sm"
                  className="h-7 gap-1 text-xs"
                  disabled={poll.isPending || !watch.enabled}
                  onClick={pollNow}
                  title={
                    watch.enabled
                      ? "Poll for new signals now"
                      : "Enable the watch to poll"
                  }
                >
                  <RefreshCw
                    className={`size-3 ${poll.isPending ? "animate-spin" : ""}`}
                  />
                  Poll now
                </Button>
              </Gate>
              <Gate need="editor">
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7 gap-1 text-xs"
                  disabled={patch.isPending}
                  onClick={toggleEnabled}
                >
                  {watch.enabled ? (
                    <>
                      <Pause className="size-3" /> Pause
                    </>
                  ) : (
                    <>
                      <Play className="size-3" /> Enable
                    </>
                  )}
                </Button>
              </Gate>
              <Gate need="editor">
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7 gap-1 text-xs text-destructive"
                  onClick={() => setConfirmOpen(true)}
                >
                  <Trash2 className="size-3" /> Delete
                </Button>
              </Gate>
            </div>
          </div>

          {watch.last_error && (
            <div className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-[11px] text-amber-600 dark:text-amber-400">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
              <span>Last poll error: {watch.last_error}</span>
            </div>
          )}

          {/* signal_types summary */}
          <SignalTypesSummary types={watch.signal_types} />

          <Separator />

          {/* Signal feed */}
          <div className="flex flex-col gap-2">
            <h3 className="text-xs font-semibold text-muted-foreground">
              Signals feed
            </h3>
            {signalsQ.isLoading ? (
              <Loading rows={3} />
            ) : signalsQ.isError ? (
              <ErrorState
                error={signalsQ.error}
                onRetry={() => signalsQ.refetch()}
              />
            ) : signals.length === 0 ? (
              <Empty
                icon={Radar}
                title="No signals yet"
                description={
                  editAllowed
                    ? "Run “Poll now” or wait for the next scheduled poll to detect signals."
                    : "No signals have been detected for this watch yet."
                }
              />
            ) : (
              <SignalFeed signals={signals} />
            )}
          </div>

          <ConfirmDialog
            open={confirmOpen}
            onOpenChange={setConfirmOpen}
            title="Delete watch?"
            description={`This permanently deletes the watch on “${watch.target}”.`}
            confirmLabel="Delete"
            destructive
            pending={del.isPending}
            onConfirm={() =>
              del.mutate(id, {
                onSuccess: () => {
                  toast.success("Watch deleted")
                  setConfirmOpen(false)
                  onBack()
                },
              })
            }
          />
        </>
      )}
    </div>
  )
}

function SignalTypesSummary({ types }: { types: string[] }) {
  const labels = useMemo(() => types.map(signalTypeLabel), [types])
  if (labels.length === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className="text-[11px] text-muted-foreground">Signal types:</span>
      {labels.map((l) => (
        <Badge key={l} variant="outline" className="text-[10px]">
          {l}
        </Badge>
      ))}
    </div>
  )
}
