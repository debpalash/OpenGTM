import { useState, useRef, useEffect, useCallback } from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import {
  Send, Loader2, Bot, Pencil, RotateCcw, Copy, Check, X, Square,
  Sparkles, ArrowUp, Search, Building2, Zap, Globe, BarChart3, Database,
  ShieldAlert, ShieldCheck, AlertTriangle, ChevronDown, ChevronRight
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { useConversationMessages } from "@/lib/hooks"
import { streamChat, type ChatMessage, type ToolCall, type ApprovedToolCall } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { TaskDetailCard } from "@/components/task-detail-card"
import { HybridMessage } from "@/components/openui-renderer"
import { AutopilotPlanCard, type AutopilotPlan } from "@/components/autopilot-plan-card"
import { toast } from "sonner"



// ── Tool Result Card ──────────────────────────────────────────────

function ChatToolResult({ toolData, content }: { toolData: string; content: string }) {
  try {
    const data = typeof toolData === "string" ? JSON.parse(toolData) : toolData
    const jobId = data?.job_id || data?.result?.job_id
    if (jobId) {
      return (
        <div className="space-y-2">
          <TaskDetailCard jobId={jobId} compact />
          {content && <div className="text-xs text-muted-foreground">{content}</div>}
        </div>
      )
    }
  } catch { /* fall through */ }
  return <span className="text-xs">{content}</span>
}

// ── Tool Execution Indicator ──────────────────────────────────────

function ToolIndicator({ toolName }: { toolName: string }) {
  const toolIcons: Record<string, typeof Search> = {
    search_leads: Search,
    ambitionbox_search: Building2,
    ambitionbox_jobs: BarChart3,
    start_collection: Zap,
    enrich_lead: Sparkles,
    scrape_website: Globe,
    get_lead_stats: BarChart3,
  }

  const Icon = toolIcons[toolName.replace("Running tool: ", "").replace("...", "")] || Zap
  const label = toolName.replace("Running tool: ", "").replace("...", "").replace(/_/g, " ")

  return (
    <div className="inline-flex items-center gap-2.5 px-3.5 py-2 rounded-xl bg-primary/5 border border-primary/10 text-sm animate-in fade-in slide-in-from-bottom-1 duration-300">
      <div className="relative">
        <Icon className="size-4 text-primary" />
        <span className="absolute -top-0.5 -right-0.5 size-2 bg-primary rounded-full animate-ping" />
      </div>
      <span className="text-muted-foreground capitalize">{label}</span>
      <Loader2 className="size-3.5 animate-spin text-primary/60" />
    </div>
  )
}

// ── Confirmation Gate ─────────────────────────────────────────────

interface ConfirmationInfo {
  confirmation_id: string
  tool_call: ToolCall
  name: string
  args: Record<string, unknown>
  label: string
  description: string
  level: "high" | "medium" | "low"
}

const LEVEL_COLORS = {
  high: "border-red-500/30 bg-red-500/5 text-red-400",
  medium: "border-amber-500/30 bg-amber-500/5 text-amber-400",
  low: "border-blue-500/30 bg-blue-500/5 text-blue-400",
} as const

function ConfirmationItem({ info }: { info: ConfirmationInfo }) {
  // Autopilot plans get a richer, structured card instead of plain text lines.
  if (info.name === "execute_plan" && info.args?.plan) {
    return <AutopilotPlanCard plan={info.args.plan as AutopilotPlan} />
  }
  const LevelIcon = info.level === "high" ? AlertTriangle : info.level === "medium" ? ShieldAlert : ShieldCheck
  const lines = info.description.split("\n").filter(Boolean)
  return (
    <div className={`flex items-start gap-3 p-3.5 rounded-xl border ${LEVEL_COLORS[info.level]}`}>
      <LevelIcon className="size-5 shrink-0 mt-0.5" />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 mb-1">
          <span className="text-sm font-medium">{lines[0] || info.label}</span>
          <span className={`px-1.5 py-0.5 text-[10px] font-medium rounded-full uppercase tracking-wider ${
            info.level === "high" ? "bg-red-500/20 text-red-300"
            : info.level === "medium" ? "bg-amber-500/20 text-amber-300"
            : "bg-blue-500/20 text-blue-300"
          }`}>{info.level} risk</span>
        </div>
        {lines.slice(1).map((line, i) => (
          <div key={i} className="text-xs text-muted-foreground">{line}</div>
        ))}
      </div>
    </div>
  )
}

// Blocking human-in-the-loop gate: the turn paused awaiting approval. Resolving
// resubmits the chat with the per-call approve/deny decisions so the backend
// executes (or skips) each dangerous tool.
function ConfirmationGate({
  confirmations, onResolve, disabled,
}: {
  confirmations: ConfirmationInfo[]
  onResolve: (decisions: Record<string, "approve" | "deny">) => void
  disabled?: boolean
}) {
  const resolveAll = (decision: "approve" | "deny") =>
    onResolve(Object.fromEntries(confirmations.map(c => [c.confirmation_id, decision])))

  return (
    <div className="space-y-2 animate-in fade-in slide-in-from-bottom-1 duration-300">
      <div className="text-xs text-muted-foreground">
        {confirmations.length > 1
          ? `${confirmations.length} actions need your approval before they run:`
          : "This action needs your approval before it runs:"}
      </div>
      {confirmations.map(c => <ConfirmationItem key={c.confirmation_id} info={c} />)}
      <div className="flex items-center gap-2 pt-0.5">
        <Button size="sm" onClick={() => resolveAll("approve")} disabled={disabled} className="gap-1.5">
          <Check className="size-3.5" /> {confirmations.length > 1 ? "Approve all" : "Approve"}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => resolveAll("deny")} disabled={disabled} className="gap-1.5">
          <X className="size-3.5" /> {confirmations.length > 1 ? "Deny all" : "Deny"}
        </Button>
      </div>
    </div>
  )
}

// ── Reasoning / Steps Timeline ────────────────────────────────────
// A live view of the agent's multi-step tool use this turn, built from the
// tool_call / tool_result / tool_denied stream events.

interface ReasoningStep {
  id: string
  title: string
  status: "running" | "done" | "denied"
  detail?: string
}

// One-line summary of a tool call's arguments, for the reasoning trace.
function summarizeArgs(args: Record<string, unknown> | undefined): string {
  if (!args) return ""
  // Prefer the most informative common args, then fall back to the first value.
  const pref = ["goal", "query", "icp_description", "company", "column_name", "status", "interval"]
  for (const k of pref) {
    const v = args[k]
    if (typeof v === "string" && v.trim()) return v.length > 60 ? v.slice(0, 57) + "…" : v
  }
  const lead = args["lead_id"]
  if (lead !== undefined) return `lead #${lead}`
  for (const v of Object.values(args)) {
    if (typeof v === "string" && v.trim()) return v.length > 60 ? v.slice(0, 57) + "…" : v
    if (typeof v === "number") return String(v)
  }
  return ""
}

// Mark the most recently-started running step with a terminal status. Tools
// run sequentially server-side, so events arrive in order and the last running
// step is the one this result/denial belongs to.
function markLastRunning(steps: ReasoningStep[], status: "done" | "denied"): ReasoningStep[] {
  for (let i = steps.length - 1; i >= 0; i--) {
    if (steps[i].status === "running") {
      const next = [...steps]
      next[i] = { ...next[i], status }
      return next
    }
  }
  return steps
}

function ReasoningTimeline({ steps }: { steps: ReasoningStep[] }) {
  const [open, setOpen] = useState(true)
  if (steps.length === 0) return null
  const working = steps.some(s => s.status === "running")
  const label = working ? "Working…" : `Used ${steps.length} tool${steps.length > 1 ? "s" : ""}`
  return (
    <div className="rounded-xl border border-border/40 bg-muted/20 mb-2">
      <button
        onClick={() => setOpen(o => !o)}
        className="flex items-center gap-2 w-full px-3 py-2 text-xs text-muted-foreground hover:text-foreground transition-colors"
      >
        {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
        <span>{label}</span>
        {working && <Loader2 className="size-3 animate-spin ml-auto text-primary/60" />}
      </button>
      {open && (
        <div className="px-3 pb-2.5 space-y-1.5">
          {steps.map(s => (
            <div key={s.id} className="flex items-start gap-2 text-xs">
              {s.status === "running"
                ? <Loader2 className="size-3 shrink-0 animate-spin text-primary mt-0.5" />
                : s.status === "denied"
                  ? <X className="size-3 shrink-0 text-red-400 mt-0.5" />
                  : <Check className="size-3 shrink-0 text-emerald-500 mt-0.5" />}
              <span className="min-w-0">
                <span className="text-muted-foreground capitalize">{s.title}</span>
                {s.detail && <span className="text-muted-foreground/50"> — {s.detail}</span>}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// ── Typing Indicator ──────────────────────────────────────────────

function TypingIndicator() {
  return (
    <div className="flex items-center gap-1.5 px-1 py-2">
      <span className="size-2 rounded-full bg-foreground/20 animate-[bounce_1.4s_ease-in-out_infinite]" />
      <span className="size-2 rounded-full bg-foreground/20 animate-[bounce_1.4s_ease-in-out_0.2s_infinite]" />
      <span className="size-2 rounded-full bg-foreground/20 animate-[bounce_1.4s_ease-in-out_0.4s_infinite]" />
    </div>
  )
}

// ── Action Buttons ────────────────────────────────────────────────

function ActionBar({ children, align = "start" }: { children: React.ReactNode; align?: "start" | "end" }) {
  return (
    <div className={`flex items-center gap-0.5 mt-1.5 opacity-0 group-hover:opacity-100 transition-all duration-200 translate-y-1 group-hover:translate-y-0 ${align === "end" ? "justify-end" : "justify-start"}`}>
      {children}
    </div>
  )
}

function ActionBtn({ icon, label, onClick }: { icon: React.ReactNode; label: string; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className="p-1.5 rounded-lg hover:bg-muted/80 text-muted-foreground/60 hover:text-foreground transition-all duration-150 active:scale-90"
      title={label}
    >
      {icon}
    </button>
  )
}

function CopyBtn({ content }: { content: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <ActionBtn
      icon={copied ? <Check className="size-3.5 text-emerald-500" /> : <Copy className="size-3.5" />}
      label={copied ? "Copied!" : "Copy"}
      onClick={() => {
        navigator.clipboard.writeText(content)
        setCopied(true)
        setTimeout(() => setCopied(false), 2000)
      }}
    />
  )
}

// ── Quick Actions (landing page) ──────────────────────────────────

const QUICK_ACTIONS = [
  { icon: <Zap className="size-4" />, text: "Build a list of 50 IT staffing firms in Pune and find their founders' emails", color: "text-rose-400" },
  { icon: <Building2 className="size-4" />, text: "Search AmbitionBox for SaaS companies", color: "text-emerald-400" },
  { icon: <BarChart3 className="size-4" />, text: "Show me my pipeline stats", color: "text-amber-400" },
  { icon: <Sparkles className="size-4" />, text: "Find hot leads missing email", color: "text-purple-400" },
]


// ── Main Chat Page ────────────────────────────────────────────────

export default function ChatPage() {
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const activeConvId = searchParams.get("id")

  const [input, setInput] = useState("")
  const [isLoading, setIsLoading] = useState(false)
  const [streamingContent, setStreamingContent] = useState("")
  const [streamingTool, setStreamingTool] = useState<string | null>(null)
  const [optimisticMessages, setOptimisticMessages] = useState<ChatMessage[]>([])
  const [editingMsgId, setEditingMsgId] = useState<string | null>(null)
  const [editText, setEditText] = useState("")
  const [streamingJobIds, setStreamingJobIds] = useState<string[]>([])
  const [confirmations, setConfirmations] = useState<ConfirmationInfo[]>([])
  const [reasoningSteps, setReasoningSteps] = useState<ReasoningStep[]>([])
  // History of the turn that paused for confirmation, replayed on approve/deny.
  const [pausedHistory, setPausedHistory] = useState<Array<{ role: string; content: string }>>([])

  const scrollRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const editRef = useRef<HTMLTextAreaElement>(null)
  const bottomRef = useRef<HTMLDivElement>(null)
  const abortRef = useRef<AbortController | null>(null)

  const { data: detailData, isLoading: isLoadingMessages } = useConversationMessages(activeConvId)

  const serverMessages = detailData?.messages || []
  const displayMessages = [...serverMessages, ...optimisticMessages]

  // Smooth scroll to bottom
  const scrollToBottom = useCallback(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" })
  }, [])

  useEffect(() => { scrollToBottom() }, [displayMessages.length, streamingContent, streamingTool, scrollToBottom])

  // Focus edit textarea
  useEffect(() => {
    if (editingMsgId && editRef.current) {
      editRef.current.focus()
      editRef.current.setSelectionRange(editRef.current.value.length, editRef.current.value.length)
    }
  }, [editingMsgId])

  // ── Core send ──

  const sendMessages = useCallback(async (
    history: Array<{ role: string; content: string }>,
    optimistic: ChatMessage[],
    opts: { approvedToolCalls?: ApprovedToolCall[] } = {},
  ) => {
    setOptimisticMessages(optimistic)
    setIsLoading(true)
    setStreamingContent("")
    setStreamingTool(null)
    setStreamingJobIds([])
    setConfirmations([])
    setReasoningSteps([])
    setPausedHistory(history)  // replayed if the turn pauses for confirmation

    const ac = new AbortController()
    abortRef.current = ac

    let currentConvId = activeConvId
    let paused = false   // turn ended awaiting confirmation — keep gate visible
    let aborted = false
    let partialContent = ""  // mirror of streamingContent for the abort path

    try {
      await streamChat(history, currentConvId, (event) => {
        if (event.conversation_id && !currentConvId) {
          currentConvId = event.conversation_id
          navigate(`/chat?id=${currentConvId}`, { replace: true })
          queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
        }
        if (event.content) { partialContent += event.content; setStreamingContent(prev => prev + event.content) }
        if (event.tool_call) {
          const tname = event.tool_call.name
          setStreamingTool(`Running tool: ${tname}...`)
          const detail = summarizeArgs(event.tool_call.args)
          setReasoningSteps(prev => [...prev, {
            id: `${tname}-${prev.length}`, title: tname.replace(/_/g, " "), status: "running", detail,
          }])
        }
        if (event.tool_result) {
          setStreamingTool(null)
          // Mark the most recent running step done (tools run sequentially).
          setReasoningSteps(prev => markLastRunning(prev, "done"))
          const jobId = event.tool_result?.result?.job_id as string | undefined
          if (jobId) setStreamingJobIds(prev => [...prev, jobId])
        }
        if (event.tool_denied) {
          setReasoningSteps(prev => markLastRunning(prev, "denied"))
        }
        if (event.confirmation_required) {
          paused = true
          setConfirmations(prev => [...prev, event.confirmation_required as ConfirmationInfo])
        }
        if (event.warning) toast.warning(event.warning)
        if (event.error) toast.error(event.error)
      }, { signal: ac.signal, approvedToolCalls: opts.approvedToolCalls })

      if (currentConvId) {
        await queryClient.invalidateQueries({ queryKey: queryKeys.conversations.detail(currentConvId) })
        queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
      }
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") {
        aborted = true
        toast("Stopped")
      } else {
        toast.error(err instanceof Error ? err.message : "Chat failed")
      }
    } finally {
      abortRef.current = null
      setIsLoading(false)
      setStreamingTool(null)
      inputRef.current?.focus()
      if (paused) {
        // Leave streamingContent + confirmations visible; the gate drives the
        // next step. Do NOT clear them here.
      } else if (aborted) {
        // Keep the partial answer in the transcript instead of discarding it.
        const partial = partialContent
        if (partial.trim()) {
          setOptimisticMessages(prev => [...prev, {
            id: crypto.randomUUID(),
            conversation_id: currentConvId || "",
            role: "assistant",
            content: partial,
            tool_data: null,
            created_at: new Date().toISOString(),
          }])
        }
        setStreamingContent("")
        setStreamingJobIds([])
        setConfirmations([])
        setReasoningSteps([])
      } else {
        setStreamingContent("")
        setStreamingJobIds([])
        setConfirmations([])
        setReasoningSteps([])
        setOptimisticMessages([])
      }
    }
  }, [activeConvId, navigate])

  // ── Send ──

  const handleSend = useCallback(async () => {
    const text = input.trim()
    if (!text || isLoading) return

    const userMsg: ChatMessage = {
      id: crypto.randomUUID(),
      conversation_id: activeConvId || "",
      role: "user",
      content: text,
      tool_data: null,
      created_at: new Date().toISOString(),
    }

    setInput("")
    const history = serverMessages.map(m => ({ role: m.role, content: m.content || "" }))
    history.push({ role: "user", content: text })
    await sendMessages(history, [userMsg])
  }, [input, isLoading, activeConvId, serverMessages, sendMessages])

  // ── Edit & resubmit ──

  const handleEditSubmit = useCallback(async (msgId: string) => {
    const text = editText.trim()
    if (!text || isLoading) return
    setEditingMsgId(null)

    const msgIndex = serverMessages.findIndex(m => m.id === msgId)
    if (msgIndex === -1) return

    const historyBefore = serverMessages.slice(0, msgIndex).map(m => ({ role: m.role, content: m.content || "" }))
    historyBefore.push({ role: "user", content: text })

    const editedMsg: ChatMessage = {
      id: crypto.randomUUID(),
      conversation_id: activeConvId || "",
      role: "user",
      content: text,
      tool_data: null,
      created_at: new Date().toISOString(),
    }

    await sendMessages(historyBefore, [editedMsg])
  }, [editText, isLoading, serverMessages, activeConvId, sendMessages])

  // ── Regenerate ──

  const handleRegenerate = useCallback(async () => {
    if (isLoading) return
    const lastUserIdx = [...serverMessages].reverse().findIndex(m => m.role === "user")
    if (lastUserIdx === -1) return
    const actualIdx = serverMessages.length - 1 - lastUserIdx
    const history = serverMessages.slice(0, actualIdx + 1).map(m => ({ role: m.role, content: m.content || "" }))
    await sendMessages(history, [])
  }, [isLoading, serverMessages, sendMessages])

  // ── Stop the in-flight stream ──
  const handleStop = useCallback(() => {
    abortRef.current?.abort()
  }, [])

  // ── Resolve a confirmation gate (approve/deny) → resubmit the paused turn ──
  const handleConfirmationResolve = useCallback(async (
    decisions: Record<string, "approve" | "deny">,
  ) => {
    const approvedToolCalls: ApprovedToolCall[] = confirmations.map(c => ({
      tool_call: c.tool_call,
      decision: decisions[c.confirmation_id] ?? "deny",
    }))
    const history = pausedHistory
    setConfirmations([])
    setStreamingContent("")
    await sendMessages(history, [], { approvedToolCalls })
  }, [confirmations, pausedHistory, sendMessages])

  useEffect(() => {
    setOptimisticMessages([])
    setStreamingContent("")
    setStreamingTool(null)
    setEditingMsgId(null)
    setConfirmations([])
    setReasoningSteps([])
    setPausedHistory([])
  }, [activeConvId])

  // Last assistant index for regenerate button
  const lastAssistantIdx = (() => {
    for (let i = displayMessages.length - 1; i >= 0; i--) {
      if (displayMessages[i].role === "assistant") return i
    }
    return -1
  })()

  // Get time of day greeting
  const greeting = (() => {
    const h = new Date().getHours()
    if (h < 12) return "Good morning"
    if (h < 17) return "Good afternoon"
    return "Good evening"
  })()

   return (
    <div className="h-full min-h-0 relative bg-background w-full overflow-hidden flex flex-col">
      <div className="flex-1 min-h-0 overflow-y-auto scroll-smooth" ref={scrollRef}>
        <div className="max-w-4xl mx-auto pb-36 pt-6 px-4">

          {/* ── Landing ── */}
          {!activeConvId && displayMessages.length === 0 && (
            <div className="flex flex-col items-center justify-center min-h-[65vh] text-center">
              <div className="mb-5 relative">
                <div className="flex size-16 items-center justify-center rounded-2xl bg-gradient-to-br from-primary/10 to-primary/5 text-primary shadow-lg shadow-primary/5">
                  <Sparkles className="size-8" />
                </div>
                <span className="absolute -bottom-1 -right-1 size-4 bg-emerald-500 rounded-full border-2 border-background" />
              </div>
              <h2 className="text-2xl font-semibold tracking-tight mb-2">{greeting}</h2>
              <p className="text-muted-foreground mb-10 max-w-sm">
                What would you like to discover today?
              </p>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2.5 max-w-xl w-full">
                {QUICK_ACTIONS.map((qa) => (
                  <button
                    key={qa.text}
                    className="flex items-center gap-3 text-left text-sm p-3.5 rounded-2xl border border-border/40 bg-card/50 hover:bg-muted/50 hover:border-border transition-all duration-200 group/qa"
                    onClick={() => { setInput(qa.text); inputRef.current?.focus() }}
                  >
                    <span className={`${qa.color} opacity-60 group-hover/qa:opacity-100 transition-opacity`}>{qa.icon}</span>
                    <span className="text-muted-foreground group-hover/qa:text-foreground transition-colors">{qa.text}</span>
                  </button>
                ))}
              </div>
            </div>
          )}

          {/* ── Loading ── */}
          {isLoadingMessages && activeConvId && displayMessages.length === 0 && (
            <div className="flex justify-center p-12">
              <Loader2 className="size-5 animate-spin text-muted-foreground/40" />
            </div>
          )}

          {/* ── Messages ── */}

          {/* Collect all job IDs from tool messages for "merge all" */}
          {(() => {
            const allJobIds = displayMessages
              .filter(m => m.role === "tool" && m.tool_data)
              .map(m => {
                try {
                  const d = typeof m.tool_data === "string" ? JSON.parse(m.tool_data) : m.tool_data
                  return d?.job_id || d?.result?.job_id || null
                } catch { return null }
              })
              .filter(Boolean) as string[]

            if (allJobIds.length < 2) return null

            return (
              <div className="flex items-center justify-between p-2.5 rounded-xl border border-primary/10 bg-primary/5 mb-4 animate-in fade-in duration-300">
                <div className="flex items-center gap-2 text-sm text-muted-foreground">
                  <Database className="size-4 text-primary" />
                  <span>{allJobIds.length} tasks in this chat</span>
                </div>
                <button
                  onClick={async () => {
                    try {
                      const { createWorkbookFromJobs } = await import("@/lib/workbook-api")
                      const wb = await createWorkbookFromJobs({ job_ids: allJobIds })
                      toast.success(`Workbook "${wb.name}" created with ${wb.total_rows} leads`)
                      navigate(`/workbooks/${wb.id}`)
                    } catch {
                      toast.error("Failed to create workbook")
                    }
                  }}
                  className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium rounded-lg bg-primary text-primary-foreground hover:bg-primary/90 transition-colors"
                >
                  <Database className="size-3" />
                  Merge all to Workbook
                </button>
              </div>
            )
          })()}

          <div className="space-y-5">
            {displayMessages.map((msg, idx) => {
              const isUser = msg.role === "user"
              const isAssistant = msg.role === "assistant"
              const isTool = msg.role === "tool"
              const isEditing = editingMsgId === msg.id
              const isLastAssistant = idx === lastAssistantIdx
              const isServerMsg = idx < serverMessages.length

              // Extract job_id from tool data for inline task cards
              const toolJobId = (() => {
                if (!isTool || !msg.tool_data) return null
                try {
                  const d = typeof msg.tool_data === "string" ? JSON.parse(msg.tool_data) : msg.tool_data
                  return d?.job_id || d?.result?.job_id || null
                } catch { return null }
              })()

              // Skip tool messages unless they have a job to track
              if (isTool && !toolJobId) return null

              return (
                <div
                  key={msg.id}
                  className="group animate-in fade-in duration-300"
                  style={{ animationDelay: `${Math.min(idx * 30, 150)}ms` }}
                >
                  {isUser ? (
                    /* ── User Message ── */
                    <div className="flex justify-end">
                      <div className="max-w-[80%] md:max-w-[70%]">
                        {isEditing ? (
                          <div className="space-y-2 animate-in fade-in zoom-in-95 duration-200">
                            <Textarea
                              ref={editRef}
                              value={editText}
                              onChange={(e) => setEditText(e.target.value)}
                              onKeyDown={(e) => {
                                if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); handleEditSubmit(msg.id) }
                                if (e.key === "Escape") setEditingMsgId(null)
                              }}
                              className="resize-none min-h-[52px] max-h-[200px] w-full rounded-2xl bg-muted border-primary/20 px-4 py-3 text-[15px] focus-visible:ring-1 focus-visible:ring-primary/30"
                              rows={2}
                            />
                            <div className="flex justify-end gap-1.5">
                              <Button size="sm" variant="ghost" onClick={() => setEditingMsgId(null)} className="h-7 px-3 text-xs rounded-full">
                                Cancel
                              </Button>
                              <Button size="sm" onClick={() => handleEditSubmit(msg.id)} disabled={!editText.trim() || isLoading}
                                className="h-7 px-3 text-xs rounded-full">
                                <Send className="size-3 mr-1.5" /> Submit
                              </Button>
                            </div>
                          </div>
                        ) : (
                          <>
                            <div className="px-4 py-2.5 rounded-[20px] bg-primary text-primary-foreground text-[15px] leading-relaxed whitespace-pre-wrap shadow-sm">
                              {msg.content}
                            </div>
                            {!isLoading && isServerMsg && (
                              <ActionBar align="end">
                                <CopyBtn content={msg.content || ""} />
                                <ActionBtn
                                  icon={<Pencil className="size-3.5" />}
                                  label="Edit message"
                                  onClick={() => { setEditingMsgId(msg.id); setEditText(msg.content || "") }}
                                />
                              </ActionBar>
                            )}
                          </>
                        )}
                      </div>
                    </div>
                  ) : isAssistant ? (
                    /* ── Assistant Message ── */
                    <div className="flex gap-3">
                      <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-primary/10 to-primary/5 text-primary mt-1 ring-1 ring-primary/10">
                        <Bot className="size-4" />
                      </div>
                      <div className="flex-1 min-w-0">
                        <div className="text-[15px] leading-relaxed">
                          <HybridMessage content={msg.content || ""} />
                        </div>
                        {!isLoading && isServerMsg && (
                          <ActionBar>
                            <CopyBtn content={msg.content || ""} />
                            {isLastAssistant && (
                              <ActionBtn
                                icon={<RotateCcw className="size-3.5" />}
                                label="Regenerate"
                                onClick={handleRegenerate}
                              />
                            )}
                          </ActionBar>
                        )}
                      </div>
                    </div>
                  ) : isTool && toolJobId ? (
                    /* ── Inline Task Progress Card ── */
                    <div className="flex gap-3">
                      <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-amber-500/10 to-amber-500/5 text-amber-500 mt-1 ring-1 ring-amber-500/10">
                        <Zap className="size-4" />
                      </div>
                      <div className="flex-1 min-w-0">
                        <TaskDetailCard jobId={toolJobId} compact />
                      </div>
                    </div>
                  ) : null}
                </div>
              )
            })}
          </div>

          {/* ── Streaming Response ── */}
          {(streamingContent || streamingTool || reasoningSteps.length > 0 || (isLoading && !streamingContent && !streamingTool)) && (
            <div className="flex gap-3 mt-5 animate-in fade-in slide-in-from-bottom-2 duration-300">
              <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-primary/10 to-primary/5 text-primary mt-1 ring-1 ring-primary/10">
                <Bot className="size-4" />
              </div>
              <div className="flex-1 min-w-0">
                <ReasoningTimeline steps={reasoningSteps} />
                {streamingTool && !streamingContent && (
                  <ToolIndicator toolName={streamingTool} />
                )}
                {streamingContent && (
                  <div className="text-[15px] leading-relaxed">
                    <HybridMessage content={streamingContent} isStreaming={true} />
                    <span className="inline-block w-[3px] h-[18px] ml-0.5 bg-primary/50 animate-pulse align-middle rounded-full" />
                  </div>
                )}
                {isLoading && !streamingContent && !streamingTool && (
                  <TypingIndicator />
                )}
              </div>
            </div>
          )}

          {/* ── Confirmation Gate (human-in-the-loop) ── */}
          {confirmations.length > 0 && (
            <div className="flex gap-3 mt-3 animate-in fade-in slide-in-from-bottom-2 duration-300">
              <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-amber-500/10 to-amber-500/5 text-amber-500 mt-1 ring-1 ring-amber-500/10">
                <ShieldAlert className="size-4" />
              </div>
              <div className="flex-1 min-w-0">
                <ConfirmationGate
                  confirmations={confirmations}
                  onResolve={handleConfirmationResolve}
                  disabled={isLoading}
                />
              </div>
            </div>
          )}

          {/* ── Inline Task Progress Cards (from streaming) ── */}
          {streamingJobIds.map((jid) => (
            <div key={jid} className="flex gap-3 mt-3 animate-in fade-in slide-in-from-bottom-2 duration-300">
              <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-amber-500/10 to-amber-500/5 text-amber-500 mt-1 ring-1 ring-amber-500/10">
                <Zap className="size-4" />
              </div>
              <div className="flex-1 min-w-0">
                <TaskDetailCard jobId={jid} compact />
              </div>
            </div>
          ))}

          <div ref={bottomRef} />
        </div>
      </div>

      {/* ── Input Area ── */}
      <div className="absolute bottom-0 left-0 right-0 bg-gradient-to-t from-background from-60% to-transparent pointer-events-none">
        <div className="max-w-4xl mx-auto px-4 pb-4 pt-8 pointer-events-auto">
          <div className="relative flex items-end w-full bg-muted/30 backdrop-blur-sm border border-border/40 rounded-[24px] focus-within:ring-2 focus-within:ring-primary/15 focus-within:border-primary/20 focus-within:bg-background/80 transition-all duration-300 shadow-lg shadow-black/[0.03]">
            <Textarea
              ref={inputRef}
              placeholder="Ask anything or search leads..."
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault()
                  handleSend()
                }
              }}
              disabled={isLoading}
              className="resize-none min-h-[48px] max-h-[200px] w-full bg-transparent border-0 focus-visible:ring-0 px-5 py-3.5 text-[15px] placeholder:text-muted-foreground/50"
              rows={1}
            />
            <div className="flex shrink-0 p-2.5">
              {isLoading ? (
                <button
                  onClick={handleStop}
                  title="Stop generating"
                  className="flex items-center justify-center h-8 w-8 rounded-full bg-foreground text-background shadow-md hover:scale-105 active:scale-95 transition-all duration-200"
                >
                  <Square className="size-3.5 fill-current" />
                </button>
              ) : (
                <button
                  onClick={handleSend}
                  disabled={!input.trim()}
                  className={`flex items-center justify-center h-8 w-8 rounded-full transition-all duration-200 ${
                    input.trim()
                      ? "bg-primary text-primary-foreground shadow-md shadow-primary/20 hover:shadow-lg hover:shadow-primary/30 hover:scale-105 active:scale-95"
                      : "bg-muted text-muted-foreground"
                  } disabled:opacity-40`}
                >
                  <ArrowUp className="size-4" />
                </button>
              )}
            </div>
          </div>
          <div className="text-center mt-2.5 text-[10px] text-muted-foreground/40 select-none">
            Yupcha Sales AI can make mistakes. Consider verifying important information.
          </div>
        </div>
      </div>
    </div>
  )
}
