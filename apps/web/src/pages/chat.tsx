import { useState, useRef, useEffect, useCallback } from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import {
  Send, Loader2, Bot, Pencil, RotateCcw, Copy, Check, X,
  Sparkles, ArrowUp, Search, Building2, Zap, Globe, BarChart3
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { useConversationMessages } from "@/lib/hooks"
import { streamChat, type ChatMessage } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { TaskDetailCard } from "@/components/task-detail-card"
import { MarkdownContent } from "@/components/markdown-content"
import { toast } from "sonner"

// ── Markdown Renderer (thin wrapper) ──────────────────────────────

function MarkdownMessage({ content }: { content: string }) {
  return <MarkdownContent content={content} />
}

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
  { icon: <Search className="size-4" />, text: "Find 50 IT staffing companies in Bangalore", color: "text-blue-400" },
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

  const scrollRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const editRef = useRef<HTMLTextAreaElement>(null)
  const bottomRef = useRef<HTMLDivElement>(null)

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
  ) => {
    setOptimisticMessages(optimistic)
    setIsLoading(true)
    setStreamingContent("")
    setStreamingTool(null)
    setStreamingJobIds([])

    let currentConvId = activeConvId

    try {
      await streamChat(history, currentConvId, (event) => {
        if (event.conversation_id && !currentConvId) {
          currentConvId = event.conversation_id
          navigate(`/chat?id=${currentConvId}`, { replace: true })
          queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
        }
        if (event.content) setStreamingContent(prev => prev + event.content)
        if (event.tool_call) setStreamingTool(`Running tool: ${event.tool_call.name}...`)
        if (event.tool_result) {
          setStreamingTool(null)
          // Capture job_id for inline task card
          const jobId = event.tool_result?.result?.job_id
          if (jobId) setStreamingJobIds(prev => [...prev, jobId])
        }
        if (event.error) toast.error(event.error)
      })

      if (currentConvId) {
        await queryClient.invalidateQueries({ queryKey: queryKeys.conversations.detail(currentConvId) })
        queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Chat failed")
    } finally {
      setIsLoading(false)
      setStreamingContent("")
      setStreamingTool(null)
      setStreamingJobIds([])
      setOptimisticMessages([])
      inputRef.current?.focus()
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

  useEffect(() => {
    setOptimisticMessages([])
    setStreamingContent("")
    setStreamingTool(null)
    setEditingMsgId(null)
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
        <div className="max-w-3xl mx-auto pb-36 pt-6 px-4">

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
                          <MarkdownMessage content={msg.content || ""} />
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
          {(streamingContent || streamingTool || (isLoading && !streamingContent && !streamingTool)) && (
            <div className="flex gap-3 mt-5 animate-in fade-in slide-in-from-bottom-2 duration-300">
              <div className="flex size-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-primary/10 to-primary/5 text-primary mt-1 ring-1 ring-primary/10">
                <Bot className="size-4" />
              </div>
              <div className="flex-1 min-w-0">
                {streamingTool && !streamingContent && (
                  <ToolIndicator toolName={streamingTool} />
                )}
                {streamingContent && (
                  <div className="text-[15px] leading-relaxed">
                    <MarkdownMessage content={streamingContent} />
                    <span className="inline-block w-[3px] h-[18px] ml-0.5 bg-primary/50 animate-pulse align-middle rounded-full" />
                  </div>
                )}
                {isLoading && !streamingContent && !streamingTool && (
                  <TypingIndicator />
                )}
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
        <div className="max-w-3xl mx-auto px-4 pb-4 pt-8 pointer-events-auto">
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
              <button
                onClick={handleSend}
                disabled={isLoading || !input.trim()}
                className={`flex items-center justify-center h-8 w-8 rounded-full transition-all duration-200 ${
                  input.trim()
                    ? "bg-primary text-primary-foreground shadow-md shadow-primary/20 hover:shadow-lg hover:shadow-primary/30 hover:scale-105 active:scale-95"
                    : "bg-muted text-muted-foreground"
                } disabled:opacity-40`}
              >
                {isLoading ? <Loader2 className="size-4 animate-spin" /> : <ArrowUp className="size-4" />}
              </button>
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
