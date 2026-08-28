// Sequences (Outreach) — rewired onto the scaffold React Query hooks (spec §1.1).
//
// Replaces the old raw-fetch page. Key fixes vs the prior code:
//  - CREATE posts the FULL CreateSequenceRequest (name/description/consent_basis/
//    steps/daily_limit/send_window_start/end/tz), not just {name, steps}.
//  - ENROLL sends EnrollLeadsRequest{lead_ids, consent_source} via the LeadPicker.
//  - /execute result is read as {enqueued:n} (not data.results.sent).
//  - Admin-only mutations (create/start/pause/execute/delete/suppression) are
//    gated with <Gate>/useCanRole; members can view + enroll only (member banner).
//  - Stats grid covers every per-status key; detail has Steps | Sends log |
//    Suppressions tabs; auto-paused banner + Resume; 400/403 detail surfaced.

import { useState } from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import { toast } from "sonner"
import {
  Send, Plus, Play, Pause, Trash2, MoreHorizontal, Clock,
  Mail, Users, Loader2, ChevronRight, Zap,
  XCircle, Timer, Hash, CheckCircle2, AlertCircle, AlertTriangle,
  Settings as SettingsIcon, Info, Ban, FileText, ShieldCheck, Copy,
  ExternalLink,
} from "lucide-react"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import { Textarea } from "@/components/ui/textarea"
import { Badge } from "@/components/ui/badge"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { StatusDot } from "@/components/status-dot"
import { Loading, Empty, ErrorState } from "@/components/states"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { LeadPicker } from "@/components/lead-picker"
import { Gate, useCanRole } from "@/components/gate"
import {
  useGroundedDraft, useGroundedDrafts,
  useSequences, useSequence, useSequenceSends, useSuppressions, useSmtpStatus,
  useCreateSequence, useDeleteSequence, useStartSequence, usePauseSequence,
  useUpdateSequence, useEnrollLeads, useExecuteSequence,
  useAddSuppression, useRemoveSuppression, useRole,
} from "@/lib/automation-hooks"
import type {
  DraftEvidenceSource, Sequence, SeqStep, SeqStats, Suppression,
} from "@/lib/api"
import { ApiError } from "@/lib/api"

// Consent options (spec §4): consent_basis on the sequence, consent_source on enroll.
const CONSENT_BASES = ["existing_customer", "opt_in", "event", "legitimate_interest", "other"]
const CONSENT_SOURCES = ["existing_customer", "opt_in", "event", "other"]

function fmtConsent(v: string) {
  return v.replace(/_/g, " ")
}

// ── Page ──────────────────────────────────────────────────────────────────

export default function OutreachPage() {
  const [view, setView] = useState<"list" | "create" | "detail">("list")
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [searchParams, setSearchParams] = useSearchParams()
  const draftId = searchParams.get("draft")

  const sequences = useSequences()
  const smtp = useSmtpStatus()
  const { isOwner } = useRole()
  const { allowed: canAdmin } = useCanRole("admin")

  const rows = sequences.data?.sequences ?? []
  const smtpStatus = smtp.data

  const openDetail = (id: string) => {
    setSelectedId(id)
    setView("detail")
  }

  const openDraft = (id: string) => {
    setView("list")
    setSelectedId(null)
    setSearchParams({ draft: id })
  }

  return (
    <div className="flex flex-col gap-4 p-4">
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-base font-semibold">
            {draftId ? <FileText className="size-4" /> : <Send className="size-4" />}
            Outreach
          </h2>
          <p className="mt-0.5 text-xs text-muted-foreground">
            {draftId
              ? "Inspect the saved message and evidence behind each personalized claim."
              : "Grounded drafts, email sequences, and delivery tracking."}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {!draftId && smtpStatus && (
            <span
              className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-[11px] ${
                smtpStatus.configured
                  ? "border-emerald-400/20 bg-emerald-400/5 text-emerald-400"
                  : "border-amber-400/20 bg-amber-400/5 text-amber-400"
              }`}
            >
              {smtpStatus.configured ? (
                <CheckCircle2 className="size-3" />
              ) : (
                <AlertCircle className="size-3" />
              )}
              {smtpStatus.configured ? `SMTP: ${smtpStatus.email}` : "SMTP not configured"}
            </span>
          )}
          {!draftId && view === "list" && (
            <Gate need="admin">
              <Button
                size="sm"
                variant="outline"
                onClick={() => setView("create")}
                className="h-7 gap-1 text-xs"
              >
                <Plus className="size-3" /> New Sequence
              </Button>
            </Gate>
          )}
        </div>
      </div>

      <Separator />

      {/* Member banner — enroll-yes / activate-no asymmetry (spec §6) */}
      {!draftId && !canAdmin && !isOwner && (
        <div className="flex items-start gap-2 rounded-md border border-sky-400/20 bg-sky-400/5 px-3 py-2 text-[11px] text-sky-300">
          <Info className="mt-0.5 size-3.5 shrink-0" />
          <span>You can view and enroll leads; activating/sending requires the admin role.</span>
        </div>
      )}

      {/* Stats bar */}
      {!draftId && view === "list" && rows.length > 0 && (
        <div className="flex flex-wrap items-center gap-4 text-xs text-muted-foreground">
          <span className="flex items-center gap-1">
            <Hash className="size-3" /> {rows.length} sequences
          </span>
        </div>
      )}

      {draftId ? (
        <GroundedDraftDetail
          id={draftId}
          onBack={() => setSearchParams({}, { replace: true })}
        />
      ) : view === "list" ? (
        <div className="space-y-6">
          <GroundedDraftList onOpen={openDraft} />
          <section aria-labelledby="sequences-heading" className="space-y-3">
            <div>
              <h3 id="sequences-heading" className="text-sm font-medium">Sequences</h3>
              <p className="mt-0.5 text-xs text-muted-foreground">
                Scheduled email programs with consent and delivery controls.
              </p>
            </div>
            <SequenceList
              rows={rows}
              isLoading={sequences.isLoading}
              isError={sequences.isError}
              error={sequences.error}
              onRetry={() => sequences.refetch()}
              onOpen={openDetail}
              onCreate={() => setView("create")}
            />
          </section>
        </div>
      ) : null}

      {!draftId && view === "create" && (
        <SequenceCreator
          onCreated={(id) => openDetail(id)}
          onCancel={() => setView("list")}
        />
      )}

      {!draftId && view === "detail" && selectedId && (
        <SequenceDetailView
          id={selectedId}
          onBack={() => {
            setView("list")
            setSelectedId(null)
          }}
        />
      )}
    </div>
  )
}

// ── Grounded drafts ──────────────────────────────────────────────────────

function GroundedDraftList({ onOpen }: { onOpen: (id: string) => void }) {
  const drafts = useGroundedDrafts()
  if (drafts.isLoading) return <Loading rows={2} />
  if (drafts.isError) {
    return <ErrorState error={drafts.error} onRetry={() => drafts.refetch()} />
  }
  const rows = drafts.data?.drafts ?? []
  if (rows.length === 0) return null

  return (
    <section aria-labelledby="grounded-drafts-heading" className="space-y-3">
      <div>
        <h3 id="grounded-drafts-heading" className="text-sm font-medium">Grounded drafts</h3>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Draft-only messages tied to an exact saved contact and public evidence.
        </p>
      </div>
      <div className="space-y-2">
        {rows.map((draft) => (
          <button
            key={draft.id}
            type="button"
            onClick={() => onOpen(draft.id)}
            className="flex w-full cursor-pointer items-center gap-3 rounded-lg border px-3 py-3 text-left transition-colors hover:bg-muted/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary">
              <FileText aria-hidden="true" className="size-4" />
            </span>
            <span className="min-w-0 flex-1">
              <span className="flex flex-wrap items-center gap-2">
                <span className="truncate text-sm font-medium">{draft.subject}</span>
                <Badge variant="secondary" className="text-[10px]">Draft only</Badge>
              </span>
              <span className="mt-0.5 block truncate text-xs text-muted-foreground">
                {draft.person_name} · {draft.to_email}
              </span>
            </span>
            <ChevronRight aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
          </button>
        ))}
      </div>
    </section>
  )
}

function GroundedDraftDetail({ id, onBack }: { id: string; onBack: () => void }) {
  const draft = useGroundedDraft(id)
  const [copied, setCopied] = useState(false)

  if (draft.isLoading) return <Loading rows={4} />
  if (draft.isError) {
    return <ErrorState error={draft.error} onRetry={() => draft.refetch()} />
  }
  if (!draft.data) return null

  const row = draft.data
  const personalized = (row.sentence_evidence ?? []).filter((item) => item.personalized)
  const sources = uniqueEvidence(personalized.flatMap((item) => item.evidence))
  const created = row.created_at ? new Date(row.created_at).toLocaleString() : "Unknown"

  const copyDraft = async () => {
    try {
      await navigator.clipboard.writeText(`Subject: ${row.subject}\n\n${row.body_text}`)
      setCopied(true)
      toast.success("Draft copied")
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      toast.error("Could not copy the draft")
    }
  }

  return (
    <article className="max-w-4xl space-y-4" aria-labelledby="draft-subject">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-2">
          <Button variant="ghost" size="sm" onClick={onBack} className="h-8 px-2 text-xs">
            ← Back to outreach
          </Button>
          <div>
            <div className="mb-1.5 flex flex-wrap items-center gap-2">
              <Badge variant="secondary">Draft only · not sent</Badge>
              <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                <ShieldCheck aria-hidden="true" className="size-3.5" />
                {row.contact_status === "verified" ? "Verified address" : "Risky address approved"}
              </span>
            </div>
            <h3 id="draft-subject" className="text-lg font-semibold leading-snug">{row.subject}</h3>
            <p className="mt-1 text-xs text-muted-foreground">Saved {created}</p>
          </div>
        </div>
        <Button variant="outline" size="sm" onClick={copyDraft} className="gap-1.5">
          <Copy aria-hidden="true" className="size-3.5" />
          {copied ? "Copied" : "Copy draft"}
        </Button>
      </div>

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1.35fr)_minmax(260px,0.65fr)]">
        <Card>
          <CardHeader className="space-y-2 pb-3">
            <div className="grid gap-2 text-xs sm:grid-cols-[72px_1fr]">
              <span className="text-muted-foreground">To</span>
              <span className="min-w-0 break-words font-medium">{row.person_name} &lt;{row.to_email}&gt;</span>
              <span className="text-muted-foreground">Role</span>
              <span>{row.title || "Partnerships"} at {row.company}</span>
              <span className="text-muted-foreground">Subject</span>
              <span className="font-medium">{row.subject}</span>
            </div>
          </CardHeader>
          <Separator />
          <CardContent className="pt-4">
            <pre className="whitespace-pre-wrap break-words font-sans text-sm leading-6 text-foreground">
              {row.body_text}
            </pre>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm">
              <ShieldCheck aria-hidden="true" className="size-4 text-primary" /> Evidence check
            </CardTitle>
            <p className="text-xs leading-5 text-muted-foreground">
              {personalized.length} personalized line{personalized.length === 1 ? "" : "s"} backed by {sources.length} saved public source{sources.length === 1 ? "" : "s"}.
            </p>
          </CardHeader>
          <CardContent className="space-y-3">
            {personalized.map((item) => (
              <div key={item.sentence_id} className="rounded-md border p-3">
                <p className="text-xs leading-5">{item.text}</p>
                <p className="mt-2 font-mono text-[10px] text-muted-foreground">
                  {item.claim_ids.join(" · ")}
                </p>
              </div>
            ))}
            <Separator />
            <div className="space-y-2">
              {sources.map((source) => (
                <a
                  key={source.source_url}
                  href={source.source_url}
                  target="_blank"
                  rel="noreferrer"
                  className="block rounded-md border p-3 transition-colors hover:bg-muted/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                >
                  <span className="flex items-start justify-between gap-2 text-xs font-medium">
                    <span>{source.label || "Public evidence"}</span>
                    <ExternalLink aria-hidden="true" className="mt-0.5 size-3 shrink-0" />
                  </span>
                  <span className="mt-1 block break-words text-[10px] leading-4 text-muted-foreground">
                    Observed {new Date(source.observed_at).toLocaleString()} · {Math.round(source.confidence * 100)}% confidence
                  </span>
                </a>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>

      {(row.send_performed || row.generic_inbox || row.is_role_address) && (
        <div role="alert" className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
          <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          This draft failed a safety invariant. Do not use it until the saved record is reviewed.
        </div>
      )}
    </article>
  )
}

function uniqueEvidence(rows: DraftEvidenceSource[]): DraftEvidenceSource[] {
  const seen = new Set<string>()
  return rows.filter((row) => {
    if (seen.has(row.source_url)) return false
    seen.add(row.source_url)
    return true
  })
}

// ── List ──────────────────────────────────────────────────────────────────

function SequenceList({
  rows, isLoading, isError, error, onRetry, onOpen, onCreate,
}: {
  rows: Sequence[]
  isLoading: boolean
  isError: boolean
  error: unknown
  onRetry: () => void
  onOpen: (id: string) => void
  onCreate: () => void
}) {
  const { allowed: canAdmin } = useCanRole("admin")
  const [confirmDelete, setConfirmDelete] = useState<Sequence | null>(null)
  const del = useDeleteSequence({
    onSuccess: () => {
      toast.success("Sequence deleted")
      setConfirmDelete(null)
    },
  })

  if (isLoading) return <Loading rows={4} />
  if (isError) return <ErrorState error={error} onRetry={onRetry} />
  if (rows.length === 0) {
    return (
      <Empty
        icon={Mail}
        title="No sequences yet"
        description="Create your first email sequence to start automated outreach to your leads."
        action={
          <Gate need="admin">
            <Button size="sm" onClick={onCreate} className="gap-1">
              <Plus className="size-3.5" /> Create Sequence
            </Button>
          </Gate>
        }
      />
    )
  }

  return (
    <>
      <div className="space-y-2">
        {rows.map((seq) => (
          <div
            key={seq.id}
            className="group flex cursor-pointer items-center gap-4 rounded-lg border px-3 py-2.5 transition-colors hover:bg-muted/30"
            onClick={() => onOpen(seq.id)}
          >
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <span className="truncate text-sm font-medium">{seq.name}</span>
                <StatusDot status={seq.status} />
                {seq.auto_paused && (
                  <Badge variant="secondary" className="gap-1 text-[10px] text-amber-400">
                    <AlertTriangle className="size-2.5" /> auto-paused
                  </Badge>
                )}
              </div>
              <div className="mt-0.5 flex items-center gap-3 text-[11px] text-muted-foreground">
                <span className="flex items-center gap-1">
                  <Clock className="size-3" /> {seq.steps.length} steps
                </span>
                {seq.consent_basis && (
                  <span className="capitalize">{fmtConsent(seq.consent_basis)}</span>
                )}
              </div>
            </div>

            <DropdownMenu>
              <DropdownMenuTrigger onClick={(e) => e.stopPropagation()}>
                <span className="rounded p-1 opacity-0 transition-colors hover:bg-muted group-hover:opacity-100">
                  <MoreHorizontal className="size-4" />
                </span>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={(e) => { e.stopPropagation(); onOpen(seq.id) }}>
                  View details
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  className="text-destructive"
                  disabled={!canAdmin}
                  onClick={(e) => { e.stopPropagation(); setConfirmDelete(seq) }}
                >
                  Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>

            <ChevronRight className="size-4 text-muted-foreground/30" />
          </div>
        ))}
      </div>

      <ConfirmDialog
        open={!!confirmDelete}
        onOpenChange={(o) => !o && setConfirmDelete(null)}
        title="Delete sequence?"
        description={
          confirmDelete
            ? `"${confirmDelete.name}" and its enrollments will be removed. This cannot be undone.`
            : undefined
        }
        confirmLabel="Delete"
        destructive
        pending={del.isPending}
        onConfirm={() => confirmDelete && del.mutate(confirmDelete.id)}
      />
    </>
  )
}

// ── Creator ───────────────────────────────────────────────────────────────

function SequenceCreator({
  onCreated, onCancel,
}: {
  onCreated: (id: string) => void
  onCancel: () => void
}) {
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  const [consentBasis, setConsentBasis] = useState("existing_customer")
  const [dailyLimit, setDailyLimit] = useState("50")
  const [windowStart, setWindowStart] = useState("9")
  const [windowEnd, setWindowEnd] = useState("17")
  const [tz, setTz] = useState("UTC")
  const [steps, setSteps] = useState<SeqStep[]>([
    { step_number: 0, subject: "", body_html: "", delay_hours: 0 },
  ])

  const create = useCreateSequence({
    onSuccess: (data) => {
      const id = (data as { id: string }).id
      toast.success(`Sequence "${name}" created`)
      onCreated(id)
    },
  })

  const addStep = () =>
    setSteps([
      ...steps,
      {
        step_number: steps.length,
        subject: "",
        body_html: "",
        delay_hours: steps.length === 1 ? 72 : 168,
      },
    ])

  const updateStep = (idx: number, field: keyof SeqStep, value: string | number) => {
    setSteps((prev) =>
      prev.map((s, i) => (i === idx ? { ...s, [field]: value } : s)),
    )
  }

  const removeStep = (idx: number) => {
    if (steps.length <= 1) return
    setSteps((prev) =>
      prev.filter((_, i) => i !== idx).map((s, i) => ({ ...s, step_number: i })),
    )
  }

  const handleCreate = () => {
    if (!name.trim()) return toast.error("Enter a sequence name")
    if (!consentBasis) return toast.error("Choose a consent basis")
    if (steps.some((s) => !s.subject.trim() || !s.body_html.trim())) {
      return toast.error("All steps need a subject and body")
    }
    const dl = parseInt(dailyLimit) || 0
    if (dl <= 0) return toast.error("Daily limit must be greater than 0")
    const ws = parseInt(windowStart)
    const we = parseInt(windowEnd)
    if (!(ws >= 0 && ws < we && we <= 24)) {
      return toast.error("Send window must satisfy 0 ≤ start < end ≤ 24")
    }
    create.mutate({
      name: name.trim(),
      description: description.trim(),
      consent_basis: consentBasis,
      steps,
      daily_limit: dl,
      send_window_start: ws,
      send_window_end: we,
      send_window_tz: tz.trim() || "UTC",
    })
  }

  return (
    <div className="max-w-2xl space-y-4">
      <div className="flex items-center gap-2">
        <Button variant="ghost" size="sm" onClick={onCancel} className="h-7 text-xs">
          ← Back
        </Button>
        <h3 className="text-sm font-medium">New Sequence</h3>
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1.5 sm:col-span-2">
          <Label htmlFor="seq-name" className="text-xs">Sequence name</Label>
          <Input
            id="seq-name"
            placeholder="e.g. Cold outreach — SaaS founders"
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="text-sm"
          />
        </div>
        <div className="space-y-1.5 sm:col-span-2">
          <Label htmlFor="seq-desc" className="text-xs">Description</Label>
          <Input
            id="seq-desc"
            placeholder="Optional internal note"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className="text-xs"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="seq-consent" className="text-xs">Consent basis</Label>
          <Select value={consentBasis} onValueChange={(v) => setConsentBasis(v ?? "")}>
            <SelectTrigger id="seq-consent" className="h-8 text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CONSENT_BASES.map((c) => (
                <SelectItem key={c} value={c} className="text-xs capitalize">
                  {fmtConsent(c)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="seq-limit" className="text-xs">Daily limit</Label>
          <Input
            id="seq-limit"
            type="number"
            min={1}
            value={dailyLimit}
            onChange={(e) => setDailyLimit(e.target.value)}
            className="h-8 text-xs"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="seq-ws" className="text-xs">Send window start (hr)</Label>
          <Input
            id="seq-ws"
            type="number"
            min={0}
            max={23}
            value={windowStart}
            onChange={(e) => setWindowStart(e.target.value)}
            className="h-8 text-xs"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="seq-we" className="text-xs">Send window end (hr)</Label>
          <Input
            id="seq-we"
            type="number"
            min={1}
            max={24}
            value={windowEnd}
            onChange={(e) => setWindowEnd(e.target.value)}
            className="h-8 text-xs"
          />
        </div>
        <div className="space-y-1.5 sm:col-span-2">
          <Label htmlFor="seq-tz" className="text-xs">Timezone</Label>
          <Input
            id="seq-tz"
            placeholder="UTC"
            value={tz}
            onChange={(e) => setTz(e.target.value)}
            className="h-8 max-w-xs text-xs"
          />
        </div>
      </div>

      <Separator />

      <div className="space-y-3">
        {steps.map((step, idx) => (
          <Card key={idx}>
            <CardHeader className="pb-2">
              <div className="flex items-center justify-between">
                <CardTitle className="flex items-center gap-2 text-xs font-medium">
                  <span className="flex size-5 items-center justify-center rounded-full bg-primary/10 text-[10px] font-bold text-primary">
                    {idx + 1}
                  </span>
                  {idx === 0 ? "Initial Email" : `Follow-up ${idx}`}
                </CardTitle>
                <div className="flex items-center gap-2">
                  {idx > 0 && (
                    <div className="flex items-center gap-1 text-[11px] text-muted-foreground">
                      <Timer className="size-3" />
                      <Input
                        type="number"
                        aria-label={`Delay hours before step ${idx + 1}`}
                        value={step.delay_hours}
                        onChange={(e) =>
                          updateStep(idx, "delay_hours", parseInt(e.target.value) || 0)
                        }
                        className="h-6 w-14 text-center text-[11px]"
                      />
                      <span>hrs after prev</span>
                    </div>
                  )}
                  {steps.length > 1 && (
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={`Remove step ${idx + 1}`}
                      onClick={() => removeStep(idx)}
                      className="size-6 p-0"
                    >
                      <Trash2 className="size-3 text-muted-foreground" />
                    </Button>
                  )}
                </div>
              </div>
            </CardHeader>
            <CardContent className="space-y-2">
              <div className="space-y-1">
                <Label htmlFor={`step-subj-${idx}`} className="text-[11px] text-muted-foreground">
                  Subject
                </Label>
                <Input
                  id={`step-subj-${idx}`}
                  placeholder="e.g. Quick question about {{company}}"
                  value={step.subject}
                  onChange={(e) => updateStep(idx, "subject", e.target.value)}
                  className="text-xs"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor={`step-body-${idx}`} className="text-[11px] text-muted-foreground">
                  Body
                </Label>
                <Textarea
                  id={`step-body-${idx}`}
                  placeholder={"Hi {{name}},\n\nVariables: {{company}}, {{name}}, {{city}}, {{website}}"}
                  value={step.body_html}
                  onChange={(e) => updateStep(idx, "body_html", e.target.value)}
                  className="min-h-[100px] font-mono text-xs"
                />
              </div>
            </CardContent>
          </Card>
        ))}

        <Button
          variant="outline"
          size="sm"
          onClick={addStep}
          className="h-8 w-full gap-1 border-dashed text-xs"
        >
          <Plus className="size-3" /> Add follow-up step
        </Button>
      </div>

      <div className="flex items-center gap-2 pt-2">
        <Gate need="admin">
          <Button onClick={handleCreate} disabled={create.isPending} className="gap-1">
            {create.isPending ? (
              <Loader2 className="size-3.5 animate-spin" />
            ) : (
              <Zap className="size-3.5" />
            )}
            Create Sequence
          </Button>
        </Gate>
        <Button variant="ghost" onClick={onCancel}>Cancel</Button>
      </div>
    </div>
  )
}

// ── Detail ────────────────────────────────────────────────────────────────

const STAT_CARDS: { key: keyof SeqStats | "total"; label: string }[] = [
  { key: "total", label: "Enrolled" },
  { key: "pending", label: "Pending" },
  { key: "scheduled", label: "Scheduled" },
  { key: "sent", label: "Sent" },
  { key: "opened", label: "Opened" },
  { key: "replied", label: "Replied" },
  { key: "bounced", label: "Bounced" },
  { key: "failed", label: "Failed" },
  { key: "skipped", label: "Skipped" },
  { key: "suppressed", label: "Suppressed" },
  { key: "completed", label: "Completed" },
]

function SequenceDetailView({ id, onBack }: { id: string; onBack: () => void }) {
  const seqQuery = useSequence(id)
  const { allowed: canAdmin } = useCanRole("admin")

  const [pickerOpen, setPickerOpen] = useState(false)
  const [consentSource, setConsentSource] = useState("existing_customer")

  // The shared hook onError already toasts apiErrorMessage(detail). For the
  // SMTP-not-configured 400 we additionally surface an inline Settings CTA
  // (the SmtpStartHint banner) — no second toast, to avoid double-toasting.
  const [startSmtpError, setStartSmtpError] = useState(false)
  const start = useStartSequence({
    onSuccess: () => {
      setStartSmtpError(false)
      toast.success("Sequence activated")
    },
    onError: (err) => {
      setStartSmtpError(err instanceof ApiError && err.status === 400 && /smtp/i.test(err.detail))
    },
  })
  const pause = usePauseSequence({ onSuccess: () => toast.success("Sequence paused") })
  const resume = useUpdateSequence({
    onSuccess: () => toast.success("Sequence resumed"),
  })
  const execute = useExecuteSequence({
    onSuccess: (data) => {
      const n = (data as { enqueued: number }).enqueued ?? 0
      toast.success(`Enqueued ${n} email${n === 1 ? "" : "s"}`)
    },
  })
  const enroll = useEnrollLeads({
    onSuccess: (data) => {
      const r = data as { enrolled: number; skipped: { lead_id: number }[] }
      const skipped = r.skipped?.length ?? 0
      toast.success(
        `Enrolled ${r.enrolled} lead${r.enrolled === 1 ? "" : "s"}` +
          (skipped ? ` · ${skipped} skipped` : ""),
      )
      setPickerOpen(false)
    },
  })

  if (seqQuery.isLoading) return <Loading rows={3} />
  if (seqQuery.isError) {
    if (seqQuery.error instanceof ApiError && seqQuery.error.status === 404) {
      toast.error("Sequence not found")
      onBack()
      return null
    }
    return <ErrorState error={seqQuery.error} onRetry={() => seqQuery.refetch()} />
  }
  if (!seqQuery.data) return null

  const seq = seqQuery.data
  const stats = seq.stats ?? ({} as SeqStats)
  const isActive = seq.status === "active"

  const statValue = (k: keyof SeqStats | "total"): number => {
    if (k === "sent") return Number(stats.sent ?? stats.emails_sent ?? 0)
    const v = stats[k]
    return typeof v === "number" ? v : 0
  }

  return (
    <div className="max-w-3xl space-y-4">
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Button variant="ghost" size="sm" onClick={onBack} className="h-7 text-xs">
            ← Back
          </Button>
          <h3 className="text-sm font-medium">{seq.name}</h3>
          <StatusDot status={seq.status} />
        </div>
        <div className="flex items-center gap-2">
          <Gate need="admin">
            <Button
              size="sm"
              variant={isActive ? "destructive" : "default"}
              onClick={() => (isActive ? pause.mutate(seq.id) : start.mutate(seq.id))}
              disabled={start.isPending || pause.isPending}
              className="h-7 gap-1 text-xs"
            >
              {isActive ? (
                <><Pause className="size-3" /> Pause</>
              ) : (
                <><Play className="size-3" /> Activate</>
              )}
            </Button>
          </Gate>
          <Gate need="admin">
            <Button
              size="sm"
              variant="outline"
              onClick={() => execute.mutate(seq.id)}
              disabled={execute.isPending || !isActive}
              className="h-7 gap-1 text-xs"
            >
              {execute.isPending ? (
                <Loader2 className="size-3 animate-spin" />
              ) : (
                <Send className="size-3" />
              )}
              Send now
            </Button>
          </Gate>
        </div>
      </div>

      {/* Auto-paused banner (spec §1.1) */}
      {seq.auto_paused && (
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-amber-400/30 bg-amber-400/5 px-3 py-2 text-xs text-amber-300">
          <span className="flex items-center gap-2">
            <AlertTriangle className="size-3.5" />
            Auto-paused after delivery issues ({seq.bounce_count} bounces ·{" "}
            {seq.complaint_count} complaints). Resolve, then resume.
          </span>
          <Gate need="admin">
            <Button
              size="sm"
              variant="outline"
              onClick={() => resume.mutate({ id: seq.id, body: { status: "active" } })}
              disabled={resume.isPending}
              className="h-6 gap-1 text-[11px]"
            >
              <Play className="size-3" /> Resume
            </Button>
          </Gate>
        </div>
      )}

      {/* SMTP-not-configured hint for activation */}
      <SmtpStartHint force={startSmtpError} />

      {/* Stats grid */}
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-6">
        {STAT_CARDS.map(({ key, label }) => (
          <div key={label} className="rounded-lg border px-3 py-2">
            <div className="flex items-center gap-1.5">
              <span className="text-lg font-semibold tabular-nums">{statValue(key)}</span>
            </div>
            <span className="text-[10px] text-muted-foreground">{label}</span>
          </div>
        ))}
      </div>

      {/* Enroll row */}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          onClick={() => setPickerOpen(true)}
          className="h-7 gap-1 text-xs"
        >
          <Users className="size-3" /> Pick leads…
        </Button>
        <div className="flex items-center gap-1.5">
          <Label htmlFor="enroll-consent" className="text-[11px] text-muted-foreground">
            Consent
          </Label>
          <Select value={consentSource} onValueChange={(v) => setConsentSource(v ?? "")}>
            <SelectTrigger id="enroll-consent" className="h-7 w-[150px] text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CONSENT_SOURCES.map((c) => (
                <SelectItem key={c} value={c} className="text-xs capitalize">
                  {fmtConsent(c)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      <LeadPicker
        open={pickerOpen}
        onOpenChange={setPickerOpen}
        pending={enroll.isPending}
        onConfirm={(leadIds) => {
          if (!consentSource) {
            toast.error("Choose a consent basis before enrolling")
            return
          }
          enroll.mutate({ id: seq.id, body: { lead_ids: leadIds, consent_source: consentSource } })
        }}
      />

      <Separator />

      {/* Tabs */}
      <Tabs defaultValue="steps">
        <TabsList>
          <TabsTrigger value="steps">Steps</TabsTrigger>
          <TabsTrigger value="sends">Sends log</TabsTrigger>
          <TabsTrigger value="suppressions">Suppressions</TabsTrigger>
        </TabsList>

        <TabsContent value="steps" className="mt-3">
          <StepsView steps={seq.steps} />
        </TabsContent>

        <TabsContent value="sends" className="mt-3">
          <SendsLog id={seq.id} />
        </TabsContent>

        <TabsContent value="suppressions" className="mt-3">
          <SuppressionsView canAdmin={canAdmin} />
        </TabsContent>
      </Tabs>
    </div>
  )
}

function SmtpStartHint({ force }: { force?: boolean }) {
  const navigate = useNavigate()
  const smtp = useSmtpStatus()
  if (!force && smtp.data?.configured !== false) return null
  return (
    <div className="flex items-center justify-between gap-2 rounded-md border border-amber-400/20 bg-amber-400/5 px-3 py-2 text-[11px] text-amber-300">
      <span className="flex items-center gap-2">
        <AlertCircle className="size-3.5" /> SMTP is not configured — activating will fail until you set it up.
      </span>
      <Button
        size="sm"
        variant="outline"
        onClick={() => navigate("/settings")}
        className="h-6 gap-1 text-[11px]"
      >
        <SettingsIcon className="size-3" /> Configure SMTP
      </Button>
    </div>
  )
}

function StepsView({ steps }: { steps: SeqStep[] }) {
  if (steps.length === 0) {
    return <Empty icon={Clock} title="No steps" description="This sequence has no steps." />
  }
  return (
    <div className="space-y-2">
      {steps.map((step, idx) => (
        <div key={idx} className="flex items-start gap-3">
          <div className="flex flex-col items-center gap-1 pt-1">
            <span className="flex size-6 items-center justify-center rounded-full bg-primary/10 text-[10px] font-bold text-primary">
              {idx + 1}
            </span>
            {idx < steps.length - 1 && <div className="h-8 w-px bg-border" />}
          </div>
          <div className="flex-1 rounded-lg border px-3 py-2">
            <div className="flex items-center justify-between">
              <span className="text-xs font-medium">{step.subject || "(No subject)"}</span>
              {idx > 0 && (
                <span className="flex items-center gap-1 text-[10px] text-muted-foreground">
                  <Timer className="size-3" /> +{step.delay_hours}h
                </span>
              )}
            </div>
            <pre className="mt-1 line-clamp-3 whitespace-pre-wrap font-sans text-[11px] text-muted-foreground">
              {step.body_html}
            </pre>
          </div>
        </div>
      ))}
    </div>
  )
}

const PAGE_SIZE = 25

function SendsLog({ id }: { id: string }) {
  const [page, setPage] = useState(0)
  const sends = useSequenceSends(id)

  if (sends.isLoading) return <Loading rows={4} />
  if (sends.isError) return <ErrorState error={sends.error} onRetry={() => sends.refetch()} />

  const all = sends.data?.sends ?? []
  if (all.length === 0) {
    return <Empty icon={Send} title="No sends yet" description="Sends appear here once the sequence runs." />
  }

  const pages = Math.ceil(all.length / PAGE_SIZE)
  const rows = all.slice(page * PAGE_SIZE, page * PAGE_SIZE + PAGE_SIZE)

  return (
    <div className="space-y-2">
      <div className="overflow-x-auto rounded-md border">
        <table className="w-full text-xs">
          <thead className="bg-muted/40 text-[11px] text-muted-foreground">
            <tr>
              <th className="px-3 py-2 text-left font-medium">To</th>
              <th className="px-3 py-2 text-left font-medium">Step</th>
              <th className="px-3 py-2 text-left font-medium">Subject</th>
              <th className="px-3 py-2 text-left font-medium">Status</th>
              <th className="px-3 py-2 text-left font-medium">Sent</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {rows.map((s) => (
              <tr key={s.id} className="hover:bg-muted/20">
                <td className="px-3 py-1.5">{s.to_email}</td>
                <td className="px-3 py-1.5 tabular-nums">{s.step_number + 1}</td>
                <td className="max-w-[220px] truncate px-3 py-1.5">{s.subject}</td>
                <td className="px-3 py-1.5">
                  <StatusDot status={s.status} />
                  {s.skip_reason && (
                    <span className="ml-1 text-[10px] text-muted-foreground">({s.skip_reason})</span>
                  )}
                </td>
                <td className="px-3 py-1.5 text-muted-foreground">
                  {s.sent_at ? new Date(s.sent_at).toLocaleString() : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {pages > 1 && (
        <div className="flex items-center justify-end gap-2 text-[11px] text-muted-foreground">
          <Button
            size="sm"
            variant="outline"
            className="h-6 text-[11px]"
            disabled={page === 0}
            onClick={() => setPage((p) => p - 1)}
          >
            Prev
          </Button>
          <span>
            {page + 1} / {pages}
          </span>
          <Button
            size="sm"
            variant="outline"
            className="h-6 text-[11px]"
            disabled={page >= pages - 1}
            onClick={() => setPage((p) => p + 1)}
          >
            Next
          </Button>
        </div>
      )}
    </div>
  )
}

function SuppressionsView({ canAdmin }: { canAdmin: boolean }) {
  const supp = useSuppressions()
  const [email, setEmail] = useState("")
  const add = useAddSuppression({
    onSuccess: () => {
      toast.success("Suppression added")
      setEmail("")
    },
  })
  const remove = useRemoveSuppression({
    onSuccess: () => toast.success("Suppression removed"),
  })

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-2">
        <div className="flex-1 space-y-1">
          <Label htmlFor="supp-email" className="text-[11px] text-muted-foreground">
            Suppress an email
          </Label>
          <Input
            id="supp-email"
            type="email"
            placeholder="address@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            className="h-8 max-w-xs text-xs"
          />
        </div>
        <Gate need="admin">
          <Button
            size="sm"
            variant="outline"
            className="h-8 gap-1 text-xs"
            disabled={!email.trim() || add.isPending}
            onClick={() => add.mutate(email.trim())}
          >
            <Ban className="size-3" /> Add
          </Button>
        </Gate>
      </div>

      {supp.isLoading ? (
        <Loading rows={3} />
      ) : supp.isError ? (
        <ErrorState error={supp.error} onRetry={() => supp.refetch()} />
      ) : (supp.data?.suppressions ?? []).length === 0 ? (
        <Empty icon={Ban} title="No suppressions" description="Suppressed addresses are skipped during sends." />
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <table className="w-full text-xs">
            <thead className="bg-muted/40 text-[11px] text-muted-foreground">
              <tr>
                <th className="px-3 py-2 text-left font-medium">Email</th>
                <th className="px-3 py-2 text-left font-medium">Reason</th>
                <th className="px-3 py-2 text-left font-medium">Source</th>
                <th className="px-3 py-2 text-right font-medium" />
              </tr>
            </thead>
            <tbody className="divide-y">
              {(supp.data?.suppressions ?? []).map((s: Suppression) => (
                <tr key={s.id} className="hover:bg-muted/20">
                  <td className="px-3 py-1.5">{s.email}</td>
                  <td className="px-3 py-1.5 text-muted-foreground">{s.reason || "—"}</td>
                  <td className="px-3 py-1.5 text-muted-foreground">
                    {s.source || "—"}
                    {s.locked && (
                      <Badge variant="secondary" className="ml-1.5 text-[10px]">locked</Badge>
                    )}
                  </td>
                  <td className="px-3 py-1.5 text-right">
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Remove suppression ${s.email}`}
                      className="size-6 p-0"
                      disabled={!canAdmin || s.locked || remove.isPending}
                      title={s.locked ? "Locked (unsubscribe/complaint) and cannot be removed." : "Remove"}
                      onClick={() => remove.mutate(s.email)}
                    >
                      <XCircle className="size-3.5 text-muted-foreground" />
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
