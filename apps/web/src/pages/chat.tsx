import { useState, useRef, useEffect } from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import {
  Send, Loader2, Bot, User, Clock
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Textarea } from "@/components/ui/textarea"
import { useConversationMessages } from "@/lib/hooks"
import { streamChat, type ChatMessage } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { toast } from "sonner"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"

function MarkdownMessage({ content }: { content: string }) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        table: ({ children }) => (
          <div className="overflow-x-auto my-2 rounded-lg border border-border/50">
            <table className="w-full text-sm">{children}</table>
          </div>
        ),
        thead: ({ children }) => (
          <thead className="bg-muted/50 text-xs uppercase tracking-wider">{children}</thead>
        ),
        th: ({ children }) => (
          <th className="px-3 py-2 text-left font-medium text-muted-foreground whitespace-nowrap">{children}</th>
        ),
        td: ({ children }) => (
          <td className="px-3 py-2 border-t border-border/30 whitespace-nowrap">{children}</td>
        ),
        strong: ({ children }) => (
          <strong className="font-semibold text-foreground">{children}</strong>
        ),
        a: ({ href, children }) => (
          <a href={href} target="_blank" rel="noopener noreferrer" className="text-primary underline underline-offset-2 hover:text-primary/80">
            {children}
          </a>
        ),
        ul: ({ children }) => <ul className="list-disc list-inside space-y-0.5 my-1">{children}</ul>,
        ol: ({ children }) => <ol className="list-decimal list-inside space-y-0.5 my-1">{children}</ol>,
        code: ({ children, className }) => {
          const isInline = !className
          return isInline ? (
            <code className="bg-muted px-1.5 py-0.5 rounded text-[13px] font-mono">{children}</code>
          ) : (
            <pre className="bg-muted/70 rounded-lg p-3 overflow-x-auto my-2">
              <code className="text-[13px] font-mono">{children}</code>
            </pre>
          )
        },
        h1: ({ children }) => <h1 className="text-lg font-bold mt-3 mb-1">{children}</h1>,
        h2: ({ children }) => <h2 className="text-base font-semibold mt-2 mb-1">{children}</h2>,
        h3: ({ children }) => <h3 className="text-sm font-semibold mt-2 mb-0.5">{children}</h3>,
        p: ({ children }) => <p className="mb-1.5 last:mb-0">{children}</p>,
        blockquote: ({ children }) => (
          <blockquote className="border-l-2 border-primary/30 pl-3 my-2 text-muted-foreground italic">{children}</blockquote>
        ),
      }}
    >
      {content}
    </ReactMarkdown>
  )
}

const EXAMPLES = [
  "Find 50 IT staffing companies in Bangalore",
  "Get SaaS founders in India with LinkedIn",
  "Search ecommerce brands in Mumbai",
  "Show me my hottest leads",
]

export default function ChatPage() {
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const activeConvId = searchParams.get("id")
  
  const [input, setInput] = useState("")
  const [isLoading, setIsLoading] = useState(false)
  const [streamingContent, setStreamingContent] = useState("")
  const [streamingTool, setStreamingTool] = useState<string | null>(null)
  const [optimisticMessages, setOptimisticMessages] = useState<ChatMessage[]>([])

  const scrollRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)

  const { data: detailData, isLoading: isLoadingMessages } = useConversationMessages(activeConvId)

  // Merge server messages with optimistic ones
  const serverMessages = detailData?.messages || []
  const displayMessages = [...serverMessages, ...optimisticMessages]

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [displayMessages, streamingContent, streamingTool])

  const handleSend = async () => {
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

    setOptimisticMessages(prev => [...prev, userMsg])
    setInput("")
    setIsLoading(true)
    setStreamingContent("")
    setStreamingTool(null)

    // Build history for the LLM
    const history = serverMessages.map(m => ({
      role: m.role,
      content: m.content || "",
    }))
    history.push({ role: "user", content: text })

    let currentConvId = activeConvId

    try {
      await streamChat(history, currentConvId, (event) => {
        if (event.conversation_id && !currentConvId) {
          currentConvId = event.conversation_id
          navigate(`/chat?id=${currentConvId}`, { replace: true })
          // Invalidate list to show new conversation in sidebar
          queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
        }
        if (event.content) {
          setStreamingContent(prev => prev + event.content)
        }
        if (event.tool_call) {
          setStreamingTool(`Running tool: ${event.tool_call.name}...`)
        }
        if (event.tool_result) {
          setStreamingTool(null) // Tool finished
        }
        if (event.error) {
          toast.error(event.error)
        }
      })

      // Reload messages from server to get the final assistant message + memory extraction
      if (currentConvId) {
        await queryClient.invalidateQueries({ queryKey: queryKeys.conversations.detail(currentConvId) })
        // Invalidate list to update timestamp
        queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Chat failed")
    } finally {
      setIsLoading(false)
      setStreamingContent("")
      setStreamingTool(null)
      setOptimisticMessages([])
      inputRef.current?.focus()
    }
  }

  useEffect(() => {
    // If conversation changes from URL, clear optimistic messages
    setOptimisticMessages([])
    setStreamingContent("")
    setStreamingTool(null)
  }, [activeConvId])

  return (
    <div className="flex-1 min-h-0 relative bg-background w-full">
      <div className="h-full w-full overflow-y-auto" ref={scrollRef}>
        <div className="max-w-3xl mx-auto space-y-6 pb-28 pt-8 px-4">
            {!activeConvId && displayMessages.length === 0 && (
              <div className="flex flex-col items-center justify-center min-h-[60vh] text-center animate-in fade-in duration-500">
                <div className="mb-6 flex size-14 items-center justify-center rounded-2xl bg-primary/5 text-primary">
                  <Bot className="size-7" />
                </div>
                <h2 className="text-3xl font-semibold tracking-tight mb-3">Good afternoon</h2>
                <p className="text-muted-foreground text-lg mb-10 max-w-md mx-auto">
                  How can I help you accelerate your lead generation today?
                </p>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-3 max-w-2xl mx-auto w-full">
                  {EXAMPLES.map((ex) => (
                    <button
                      key={ex}
                      className="text-left text-sm p-4 rounded-xl border bg-card hover:bg-accent/50 hover:border-accent transition-all duration-200 shadow-sm"
                      onClick={() => { setInput(ex); inputRef.current?.focus() }}
                    >
                      {ex}
                    </button>
                  ))}
                </div>
              </div>
            )}

            {isLoadingMessages && activeConvId && displayMessages.length === 0 && (
              <div className="flex justify-center p-8">
                <Loader2 className="size-6 animate-spin text-muted-foreground" />
              </div>
            )}

            {displayMessages.map((msg) => (
              <div
                key={msg.id}
                className={`flex gap-4 animate-in fade-in slide-in-from-bottom-2 duration-300 ${msg.role === "user" ? "justify-end" : "justify-start"}`}
              >
                {msg.role !== "user" && (
                  <div className="flex size-8 shrink-0 items-center justify-center rounded-full bg-primary/5 text-primary mt-1">
                    {msg.role === "assistant" ? <Bot className="size-5" /> : <Clock className="size-5" />}
                  </div>
                )}
                <div className={`max-w-[85%] md:max-w-[75%] ${msg.role === "user" ? "order-first" : ""}`}>
                  <div
                    className={`px-4 py-3 text-base whitespace-pre-wrap leading-relaxed ${
                      msg.role === "user"
                        ? "rounded-3xl bg-muted/70 text-foreground"
                        : msg.role === "tool"
                        ? "rounded-xl bg-accent/50 font-mono text-xs p-3"
                        : "text-foreground"
                    }`}
                  >
                    {msg.role === "assistant" ? (
                      <MarkdownMessage content={msg.content || ""} />
                    ) : (
                      msg.content
                    )}
                  </div>
                </div>
              </div>
            ))}

            {/* Streaming Message Indicator */}
            {(streamingContent || streamingTool) && (
              <div className="flex gap-4 justify-start animate-in fade-in duration-300">
                <div className="flex size-8 shrink-0 items-center justify-center rounded-full bg-primary/5 text-primary mt-1">
                  <Bot className="size-5" />
                </div>
                <div className="max-w-[85%] md:max-w-[75%]">
                  <div className="px-4 py-3 text-base leading-relaxed text-foreground">
                    {streamingContent && (
                      <MarkdownMessage content={streamingContent} />
                    )}
                    {!streamingContent && streamingTool && (
                      <span className="text-muted-foreground flex items-center gap-2 text-sm mt-1">
                        <Loader2 className="size-4 animate-spin" />
                        {streamingTool}
                      </span>
                    )}
                    {streamingContent && !streamingTool && (
                      <span className="inline-block w-2 h-4 ml-1 bg-primary/60 animate-pulse align-middle rounded-sm" />
                    )}
                  </div>
                </div>
              </div>
            )}
          </div>
      </div>

        {/* Input Area */}
        <div className="absolute bottom-0 left-0 right-0 p-4 bg-gradient-to-t from-background via-background to-transparent pt-6">
          <div className="max-w-3xl mx-auto relative group">
            <div className="relative flex items-end w-full bg-muted/40 border border-border/50 rounded-3xl focus-within:ring-2 focus-within:ring-ring/20 focus-within:bg-background transition-all shadow-sm">
              <Textarea
                ref={inputRef}
                placeholder="Ask anything or search leads..."
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    handleSend();
                  }
                }}
                disabled={isLoading}
                className="resize-none min-h-[48px] max-h-[200px] w-full bg-transparent border-0 focus-visible:ring-0 px-4 py-3.5 text-sm placeholder:text-muted-foreground/70"
                rows={1}
              />
              <div className="flex shrink-0 p-2">
                <Button
                  size="icon"
                  onClick={handleSend}
                  disabled={isLoading || !input.trim()}
                  className="h-8 w-8 rounded-full transition-all hover:scale-105 active:scale-95"
                >
                  {isLoading ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4 ml-0.5" />}
                </Button>
              </div>
            </div>
            <div className="text-center mt-2 text-[10px] text-muted-foreground/60">
              LeadEngine AI can make mistakes. Consider verifying important information.
            </div>
          </div>
        </div>
    </div>
  )
}
