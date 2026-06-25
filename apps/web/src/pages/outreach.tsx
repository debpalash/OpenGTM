import { useState, useEffect } from "react"
import { toast } from "sonner"
import {
  Send, Plus, Play, Pause, Trash2, MoreHorizontal, Clock,
  Mail, Users, CheckCircle2, AlertCircle, Loader2,
  ChevronRight, Zap, Eye, MousePointerClick,
  XCircle, Timer, Hash,
} from "lucide-react"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import { Textarea } from "@/components/ui/textarea"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"

// ── Types ────────────────────────────────────────────────────

interface SequenceStep {
  step_number: number
  subject: string
  body_html: string
  delay_hours: number
}

interface Sequence {
  id: string
  name: string
  description: string
  status: string
  steps_count: number
  total_leads: number
  total_sent: number
  total_opened: number
  total_replied: number
  total_bounced: number
  daily_limit: number
  created_at: number
}

interface SequenceDetail extends Sequence {
  steps: SequenceStep[]
  stats: Record<string, number>
  send_window_start: number
  send_window_end: number
}

interface SMTPStatus {
  configured: boolean
  host: string | null
  email: string | null
  from_name: string
  max_per_hour: number
}

// ── Component ────────────────────────────────────────────────

export default function OutreachPage() {
  const [sequences, setSequences] = useState<Sequence[]>([])
  const [loading, setLoading] = useState(true)
  const [smtpStatus, setSmtpStatus] = useState<SMTPStatus | null>(null)
  const [selectedSeq, setSelectedSeq] = useState<SequenceDetail | null>(null)
  const [view, setView] = useState<"list" | "detail" | "create">("list")

  const fetchSequences = async () => {
    try {
      const res = await fetch("/api/outreach/sequences")
      const data = await res.json()
      setSequences(data.sequences || [])
    } catch { /* ignore */ }
    setLoading(false)
  }

  const fetchSMTPStatus = async () => {
    try {
      const res = await fetch("/api/outreach/smtp/status")
      setSmtpStatus(await res.json())
    } catch { /* ignore */ }
  }

  useEffect(() => {
    fetchSequences()
    fetchSMTPStatus()
  }, [])

  const openDetail = async (seqId: string) => {
    try {
      const res = await fetch(`/api/outreach/sequences/${seqId}`)
      setSelectedSeq(await res.json())
      setView("detail")
    } catch {
      toast.error("Failed to load sequence")
    }
  }

  return (
    <div className="flex flex-col gap-4 p-4">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold flex items-center gap-2">
            <Send className="size-4" />
            Outreach
          </h2>
          <p className="text-xs text-muted-foreground mt-0.5">
            Email sequences, SMTP delivery, and engagement tracking.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {smtpStatus && (
            <span className={`inline-flex items-center gap-1.5 text-[11px] px-2 py-1 rounded-md border ${
              smtpStatus.configured
                ? "text-emerald-400 border-emerald-400/20 bg-emerald-400/5"
                : "text-amber-400 border-amber-400/20 bg-amber-400/5"
            }`}>
              {smtpStatus.configured ? <CheckCircle2 className="size-3" /> : <AlertCircle className="size-3" />}
              {smtpStatus.configured ? `SMTP: ${smtpStatus.email}` : "SMTP not configured"}
            </span>
          )}
          <Button size="sm" variant="outline" onClick={() => setView("create")} className="h-7 text-xs gap-1">
            <Plus className="size-3" /> New Sequence
          </Button>
        </div>
      </div>

      <Separator />

      {/* Stats Bar */}
      <div className="flex items-center gap-4 text-xs text-muted-foreground">
        <span className="flex items-center gap-1"><Hash className="size-3" /> {sequences.length} sequences</span>
        <span className="flex items-center gap-1"><Send className="size-3 text-blue-400" /> {sequences.reduce((a, s) => a + s.total_sent, 0)} sent</span>
        <span className="flex items-center gap-1"><Eye className="size-3 text-amber-400" /> {sequences.reduce((a, s) => a + s.total_opened, 0)} opened</span>
        <span className="flex items-center gap-1"><MousePointerClick className="size-3 text-emerald-400" /> {sequences.reduce((a, s) => a + s.total_replied, 0)} replied</span>
      </div>

      {/* Views */}
      {view === "list" && (
        <SequenceList
          sequences={sequences}
          loading={loading}
          onOpen={openDetail}
          onRefresh={fetchSequences}
          onCreate={() => setView("create")}
        />
      )}

      {view === "create" && (
        <SequenceCreator
          onCreated={(id) => {
            fetchSequences()
            openDetail(id)
          }}
          onCancel={() => setView("list")}
        />
      )}

      {view === "detail" && selectedSeq && (
        <SequenceDetailView
          sequence={selectedSeq}
          onBack={() => { setView("list"); setSelectedSeq(null); fetchSequences() }}
          onRefresh={() => openDetail(selectedSeq.id)}
        />
      )}
    </div>
  )
}

// ── Sequence List ────────────────────────────────────────────

function SequenceList({ sequences, loading, onOpen, onRefresh, onCreate }: {
  sequences: Sequence[]
  loading: boolean
  onOpen: (id: string) => void
  onRefresh: () => void
  onCreate: () => void
}) {
  const deleteSeq = async (id: string) => {
    await fetch(`/api/outreach/sequences/${id}`, { method: "DELETE" })
    toast.success("Sequence deleted")
    onRefresh()
  }

  if (loading) {
    return <div className="text-sm text-muted-foreground py-8 text-center">Loading sequences...</div>
  }

  if (sequences.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-16 text-center">
        <Mail className="size-10 text-muted-foreground/20 mb-4" />
        <p className="text-sm text-muted-foreground mb-1">No sequences yet</p>
        <p className="text-xs text-muted-foreground/60 mb-4 max-w-xs">
          Create your first email sequence to start automated outreach to your leads.
        </p>
        <Button size="sm" onClick={onCreate} className="gap-1">
          <Plus className="size-3.5" /> Create Sequence
        </Button>
      </div>
    )
  }

  return (
    <div className="space-y-2">
      {sequences.map(seq => (
        <div
          key={seq.id}
          className="flex items-center gap-4 px-3 py-2.5 rounded-lg border hover:bg-muted/30 cursor-pointer transition-colors group"
          onClick={() => onOpen(seq.id)}
        >
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2">
              <span className="text-sm font-medium truncate">{seq.name}</span>
              <StatusDot status={seq.status} />
            </div>
            <div className="flex items-center gap-3 mt-0.5 text-[11px] text-muted-foreground">
              <span className="flex items-center gap-1"><Clock className="size-3" /> {seq.steps_count} steps</span>
              <span className="flex items-center gap-1"><Users className="size-3" /> {seq.total_leads} leads</span>
            </div>
          </div>

          <div className="flex items-center gap-4 text-xs text-muted-foreground">
            <span className="flex items-center gap-1"><Send className="size-3" /> {seq.total_sent}</span>
            <span className="flex items-center gap-1"><Eye className="size-3" /> {seq.total_opened}</span>
            <span className="flex items-center gap-1"><MousePointerClick className="size-3" /> {seq.total_replied}</span>
          </div>

          <DropdownMenu>
            <DropdownMenuTrigger onClick={(e) => e.stopPropagation()}>
              <span className="p-1 rounded hover:bg-muted transition-colors opacity-0 group-hover:opacity-100">
                <MoreHorizontal className="size-4" />
              </span>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={(e) => { e.stopPropagation(); onOpen(seq.id) }}>
                View Details
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                className="text-destructive"
                onClick={(e) => { e.stopPropagation(); deleteSeq(seq.id) }}
              >
                Delete
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>

          <ChevronRight className="size-4 text-muted-foreground/30" />
        </div>
      ))}
    </div>
  )
}

// ── Sequence Creator ─────────────────────────────────────────

function SequenceCreator({ onCreated, onCancel }: { onCreated: (id: string) => void; onCancel: () => void }) {
  const [name, setName] = useState("")
  const [steps, setSteps] = useState<SequenceStep[]>([
    { step_number: 0, subject: "", body_html: "", delay_hours: 0 },
  ])
  const [saving, setSaving] = useState(false)

  const addStep = () => {
    setSteps([...steps, {
      step_number: steps.length,
      subject: "",
      body_html: "",
      delay_hours: steps.length === 1 ? 72 : 168, // 3 days, then 7 days
    }])
  }

  const updateStep = (idx: number, field: keyof SequenceStep, value: string | number) => {
    const updated = [...steps]
    ;(updated[idx] as any)[field] = value
    setSteps(updated)
  }

  const removeStep = (idx: number) => {
    if (steps.length <= 1) return
    setSteps(steps.filter((_, i) => i !== idx).map((s, i) => ({ ...s, step_number: i })))
  }

  const handleCreate = async () => {
    if (!name.trim()) { toast.error("Enter a sequence name"); return }
    if (steps.some(s => !s.subject.trim() || !s.body_html.trim())) {
      toast.error("All steps need a subject and body"); return
    }

    setSaving(true)
    try {
      const res = await fetch("/api/outreach/sequences", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, steps }),
      })
      const data = await res.json()
      toast.success(`Sequence "${name}" created`)
      onCreated(data.id)
    } catch {
      toast.error("Failed to create sequence")
    }
    setSaving(false)
  }

  return (
    <div className="space-y-4 max-w-2xl">
      <div className="flex items-center gap-2">
        <Button variant="ghost" size="sm" onClick={onCancel} className="h-7 text-xs">← Back</Button>
        <h3 className="text-sm font-medium">New Sequence</h3>
      </div>

      <div className="space-y-1.5">
        <Label className="text-xs">Sequence Name</Label>
        <Input
          placeholder="e.g. Cold outreach — SaaS founders"
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="text-sm"
        />
      </div>

      <Separator />

      <div className="space-y-3">
        {steps.map((step, idx) => (
          <Card key={idx}>
            <CardHeader className="pb-2">
              <div className="flex items-center justify-between">
                <CardTitle className="text-xs font-medium flex items-center gap-2">
                  <span className="size-5 rounded-full bg-primary/10 text-primary flex items-center justify-center text-[10px] font-bold">
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
                        value={step.delay_hours}
                        onChange={(e) => updateStep(idx, "delay_hours", parseInt(e.target.value) || 0)}
                        className="w-14 h-6 text-[11px] text-center"
                      />
                      <span>hrs after prev</span>
                    </div>
                  )}
                  {steps.length > 1 && (
                    <Button variant="ghost" size="sm" onClick={() => removeStep(idx)} className="h-6 w-6 p-0">
                      <Trash2 className="size-3 text-muted-foreground" />
                    </Button>
                  )}
                </div>
              </div>
            </CardHeader>
            <CardContent className="space-y-2">
              <div className="space-y-1">
                <Label className="text-[11px] text-muted-foreground">Subject</Label>
                <Input
                  placeholder="e.g. Quick question about {{company}}"
                  value={step.subject}
                  onChange={(e) => updateStep(idx, "subject", e.target.value)}
                  className="text-xs"
                />
              </div>
              <div className="space-y-1">
                <Label className="text-[11px] text-muted-foreground">Body</Label>
                <Textarea
                  placeholder={"Hi {{name}},\n\nI noticed {{company}} is growing fast...\n\nVariables: {{company}}, {{name}}, {{city}}, {{website}}, {{specialization}}"}
                  value={step.body_html}
                  onChange={(e) => updateStep(idx, "body_html", e.target.value)}
                  className="text-xs min-h-[100px] font-mono"
                />
              </div>
            </CardContent>
          </Card>
        ))}

        <Button variant="outline" size="sm" onClick={addStep} className="w-full h-8 text-xs gap-1 border-dashed">
          <Plus className="size-3" /> Add Follow-up Step
        </Button>
      </div>

      <div className="flex items-center gap-2 pt-2">
        <Button onClick={handleCreate} disabled={saving} className="gap-1">
          {saving ? <Loader2 className="size-3.5 animate-spin" /> : <Zap className="size-3.5" />}
          Create Sequence
        </Button>
        <Button variant="ghost" onClick={onCancel}>Cancel</Button>
      </div>
    </div>
  )
}

// ── Sequence Detail View ─────────────────────────────────────

function SequenceDetailView({ sequence, onBack, onRefresh }: {
  sequence: SequenceDetail
  onBack: () => void
  onRefresh: () => void
}) {
  const [enrolling, setEnrolling] = useState(false)
  const [enrollTier, setEnrollTier] = useState("hot")
  const [executing, setExecuting] = useState(false)

  const toggleStatus = async () => {
    const endpoint = sequence.status === "active" ? "pause" : "start"
    try {
      const res = await fetch(`/api/outreach/sequences/${sequence.id}/${endpoint}`, { method: "POST" })
      if (!res.ok) {
        const data = await res.json()
        toast.error(data.detail || "Failed")
        return
      }
      toast.success(endpoint === "start" ? "Sequence activated" : "Sequence paused")
      onRefresh()
    } catch {
      toast.error("Failed to update sequence")
    }
  }

  const enrollLeads = async () => {
    setEnrolling(true)
    try {
      const leadsRes = await fetch(`/api/leads?limit=100&tier=${enrollTier}`)
      const leadsData = await leadsRes.json()
      const leadRows = Array.isArray(leadsData) ? leadsData : (leadsData.leads || [])
      const withEmail = leadRows.filter((l: any) => l.email?.includes("@"))
      const ids = withEmail.map((l: any) => l.id)

      if (ids.length === 0) {
        toast.error(`No ${enrollTier} leads with emails found`)
        setEnrolling(false)
        return
      }

      const res = await fetch(`/api/outreach/sequences/${sequence.id}/enroll`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ lead_ids: ids }),
      })
      const data = await res.json()
      toast.success(`Enrolled ${data.enrolled} leads`)
      onRefresh()
    } catch {
      toast.error("Failed to enroll leads")
    }
    setEnrolling(false)
  }

  const executeNow = async () => {
    setExecuting(true)
    try {
      const res = await fetch(`/api/outreach/sequences/${sequence.id}/execute`, { method: "POST" })
      const data = await res.json()
      toast.success(`Sent: ${data.results?.sent || 0}, Failed: ${data.results?.failed || 0}`)
      onRefresh()
    } catch {
      toast.error("Execution failed")
    }
    setExecuting(false)
  }

  const stats = sequence.stats || {}

  return (
    <div className="space-y-4 max-w-3xl">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <Button variant="ghost" size="sm" onClick={onBack} className="h-7 text-xs">← Back</Button>
          <h3 className="text-sm font-medium">{sequence.name}</h3>
          <StatusDot status={sequence.status} />
        </div>
        <div className="flex items-center gap-2">
          <Button
            size="sm"
            variant={sequence.status === "active" ? "destructive" : "default"}
            onClick={toggleStatus}
            className="h-7 text-xs gap-1"
          >
            {sequence.status === "active"
              ? <><Pause className="size-3" /> Pause</>
              : <><Play className="size-3" /> Activate</>}
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={executeNow}
            disabled={executing || sequence.status !== "active"}
            className="h-7 text-xs gap-1"
          >
            {executing ? <Loader2 className="size-3 animate-spin" /> : <Send className="size-3" />}
            Send Now
          </Button>
        </div>
      </div>

      {/* Stats Cards */}
      <div className="grid grid-cols-5 gap-2">
        {[
          { label: "Enrolled", value: stats.total || 0, icon: Users, color: "text-muted-foreground" },
          { label: "Sent", value: stats.emails_sent || 0, icon: Send, color: "text-blue-400" },
          { label: "Opened", value: stats.opened || 0, icon: Eye, color: "text-amber-400" },
          { label: "Replied", value: stats.replied || 0, icon: MousePointerClick, color: "text-emerald-400" },
          { label: "Bounced", value: stats.bounced || 0, icon: XCircle, color: "text-red-400" },
        ].map(({ label, value, icon: Icon, color }) => (
          <div key={label} className="rounded-lg border px-3 py-2">
            <div className="flex items-center gap-1.5">
              <Icon className={`size-3.5 ${color}`} />
              <span className="text-lg font-semibold tabular-nums">{value}</span>
            </div>
            <span className="text-[10px] text-muted-foreground">{label}</span>
          </div>
        ))}
      </div>

      <Separator />

      {/* Enroll Leads */}
      <div className="flex items-center gap-2">
        <Select value={enrollTier} onValueChange={(v) => setEnrollTier(v ?? "")}>
          <SelectTrigger className="w-[120px] h-7 text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="hot">Hot leads</SelectItem>
            <SelectItem value="warm">Warm leads</SelectItem>
            <SelectItem value="cold">Cold leads</SelectItem>
          </SelectContent>
        </Select>
        <Button size="sm" variant="outline" onClick={enrollLeads} disabled={enrolling} className="h-7 text-xs gap-1">
          {enrolling ? <Loader2 className="size-3 animate-spin" /> : <Plus className="size-3" />}
          Enroll Leads
        </Button>
      </div>

      {/* Steps */}
      <div className="space-y-2">
        <h4 className="text-xs font-medium text-muted-foreground">Sequence Steps</h4>
        {sequence.steps.map((step, idx) => (
          <div key={idx} className="flex items-start gap-3">
            <div className="flex flex-col items-center gap-1 pt-1">
              <span className="size-6 rounded-full bg-primary/10 text-primary flex items-center justify-center text-[10px] font-bold">
                {idx + 1}
              </span>
              {idx < sequence.steps.length - 1 && (
                <div className="w-px h-8 bg-border" />
              )}
            </div>
            <div className="flex-1 rounded-lg border px-3 py-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium">{step.subject || "(No subject)"}</span>
                {idx > 0 && (
                  <span className="text-[10px] text-muted-foreground flex items-center gap-1">
                    <Timer className="size-3" /> +{step.delay_hours}h
                  </span>
                )}
              </div>
              <pre className="text-[11px] text-muted-foreground mt-1 whitespace-pre-wrap font-sans line-clamp-3">
                {step.body_html}
              </pre>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

// ── Status Dot ───────────────────────────────────────────────

function StatusDot({ status }: { status: string }) {
  const colors: Record<string, string> = {
    draft: "bg-zinc-400",
    active: "bg-emerald-400",
    paused: "bg-amber-400",
    completed: "bg-blue-400",
  }
  return (
    <span className="inline-flex items-center gap-1 text-[10px] text-muted-foreground">
      <span className={`size-1.5 rounded-full ${colors[status] || colors.draft}`} />
      {status}
    </span>
  )
}
