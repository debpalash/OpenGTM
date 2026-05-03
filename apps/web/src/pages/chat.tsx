import { useState, useRef, useEffect } from "react"
import {
  Send, Loader2, Bot, User, Sparkles,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Input } from "@/components/ui/input"
import { useCollect } from "@/lib/hooks"
import { toast } from "sonner"

interface Message {
  id: string
  role: "user" | "assistant" | "system"
  content: string
  timestamp: Date
  action?: { type: string; data: Record<string, unknown> }
}

const EXAMPLES = [
  "Find 50 IT staffing companies in Bangalore",
  "Get SaaS founders in India with LinkedIn",
  "Search ecommerce brands in Mumbai",
  "Show me my hottest leads",
]

export default function ChatPage() {
  const [messages, setMessages] = useState<Message[]>([
    {
      id: "welcome",
      role: "assistant",
      content: "I'm your AI lead engine. Tell me what companies you're looking for and I'll find, enrich, and score them for you.\n\nTry: \"Find HR consultancies in Pune\" or \"Get 100 SaaS companies in Delhi\"",
      timestamp: new Date(),
    },
  ])
  const [input, setInput] = useState("")
  const [isLoading, setIsLoading] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const collect = useCollect()

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [messages])

  const handleSend = async () => {
    const text = input.trim()
    if (!text || isLoading) return

    const userMsg: Message = {
      id: crypto.randomUUID(),
      role: "user",
      content: text,
      timestamp: new Date(),
    }
    setMessages(prev => [...prev, userMsg])
    setInput("")
    setIsLoading(true)

    // Parse intent — for now, treat all messages as collection queries
    try {
      const result = await collect.mutateAsync({ query: text })
      const assistantMsg: Message = {
        id: crypto.randomUUID(),
        role: "assistant",
        content: `Started collection pipeline for "${text}". Job ID: \`${result.job_id}\`\n\nI'll find companies matching your query, extract contact data from their websites, validate them with AI, and score them against your ICP.\n\nCheck the **Agents** tab to see live progress.`,
        timestamp: new Date(),
        action: { type: "collect", data: { job_id: result.job_id, query: text } },
      }
      setMessages(prev => [...prev, assistantMsg])
      toast.success("Pipeline started")
    } catch (err) {
      const errorMsg: Message = {
        id: crypto.randomUUID(),
        role: "assistant",
        content: `Failed to start collection: ${err instanceof Error ? err.message : "Unknown error"}`,
        timestamp: new Date(),
      }
      setMessages(prev => [...prev, errorMsg])
    }
    setIsLoading(false)
    inputRef.current?.focus()
  }

  return (
    <div className="flex flex-col h-full">
      {/* Messages */}
      <ScrollArea className="flex-1 p-4" ref={scrollRef}>
        <div className="max-w-2xl mx-auto space-y-4 pb-4">
          {messages.map((msg) => (
            <div
              key={msg.id}
              className={`flex gap-3 ${msg.role === "user" ? "justify-end" : ""}`}
            >
              {msg.role === "assistant" && (
                <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-muted">
                  <Bot className="size-4" />
                </div>
              )}
              <div className={`max-w-[80%] ${msg.role === "user" ? "order-first" : ""}`}>
                <div
                  className={`rounded-lg px-3 py-2 text-sm ${
                    msg.role === "user"
                      ? "bg-primary text-primary-foreground"
                      : "bg-muted"
                  }`}
                >
                  {msg.content.split("\n").map((line, i) => (
                    <p key={i} className={i > 0 ? "mt-1.5" : ""}>
                      {line.split(/(`[^`]+`)/).map((part, j) =>
                        part.startsWith("`") && part.endsWith("`") ? (
                          <code key={j} className="px-1 py-0.5 rounded bg-background/50 text-xs font-mono">
                            {part.slice(1, -1)}
                          </code>
                        ) : part.split(/(\*\*[^*]+\*\*)/).map((subpart, k) =>
                          subpart.startsWith("**") && subpart.endsWith("**") ? (
                            <strong key={k}>{subpart.slice(2, -2)}</strong>
                          ) : subpart
                        )
                      )}
                    </p>
                  ))}
                </div>
                {msg.action?.type === "collect" && (
                  <div className="mt-2">
                    <Badge variant="outline" className="text-xs">
                      <Sparkles className="size-3 mr-1" />
                      Pipeline running
                    </Badge>
                  </div>
                )}
                <div className="text-xs text-muted-foreground mt-1">
                  {msg.timestamp.toLocaleTimeString()}
                </div>
              </div>
              {msg.role === "user" && (
                <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-primary/10">
                  <User className="size-4" />
                </div>
              )}
            </div>
          ))}
          {isLoading && (
            <div className="flex gap-3">
              <div className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-muted">
                <Bot className="size-4" />
              </div>
              <div className="rounded-lg px-3 py-2 bg-muted">
                <Loader2 className="size-4 animate-spin" />
              </div>
            </div>
          )}
        </div>
      </ScrollArea>

      {/* Examples */}
      {messages.length <= 1 && (
        <div className="px-4 pb-2">
          <div className="max-w-2xl mx-auto flex flex-wrap gap-2">
            {EXAMPLES.map((ex) => (
              <button
                key={ex}
                className="text-xs px-3 py-1.5 rounded-full border hover:bg-muted transition-colors text-muted-foreground"
                onClick={() => { setInput(ex); inputRef.current?.focus() }}
              >
                {ex}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Input */}
      <div className="border-t p-4">
        <div className="max-w-2xl mx-auto flex items-center gap-2">
          <Input
            ref={inputRef}
            placeholder="Tell me what leads to find..."
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && !e.shiftKey && handleSend()}
            disabled={isLoading}
            autoFocus
          />
          <Button
            size="sm"
            onClick={handleSend}
            disabled={isLoading || !input.trim()}
          >
            {isLoading ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4" />}
          </Button>
        </div>
      </div>
    </div>
  )
}
