/**
 * Empty-state chat: the entry point to everything OpenGTM does. Sky backdrop
 * and ink mascot from the sign-in page, the composer with action pills and
 * `/` commands, suggestion cards, and a way back into recent conversations.
 */
import { Link } from "react-router-dom"
import { ArrowUpRight, MessageSquare } from "lucide-react"
import { useConversations, useStats } from "@/lib/hooks"
import { ChatMascot } from "./chat-mascot"
import { SUGGESTIONS, type ChatAction } from "./commands"
import "./chat-home.css"

function greeting() {
  const h = new Date().getHours()
  return h < 12 ? "Good morning" : h < 17 ? "Good afternoon" : "Good evening"
}

export function ChatSky() {
  return (
    <div className="gtm-chat-sky pointer-events-none absolute inset-0 overflow-hidden" aria-hidden="true">
      <svg className="gtm-chat-arcs absolute left-1/2 top-[46%] h-[1400px] w-[1400px] -translate-x-1/2 -translate-y-1/2" viewBox="0 0 1400 1400">
        {[260, 400, 540, 680].map(r => <circle key={r} cx="700" cy="700" r={r} />)}
      </svg>
      <div className="gtm-chat-clouds" />
    </div>
  )
}

export function ChatHome({ composer, onAction }: { composer: React.ReactNode; onAction: (action: ChatAction) => void }) {
  const { data: conversations } = useConversations()
  const { data: stats } = useStats()
  const recent = (conversations ?? []).slice(0, 3)

  return (
    <div className="relative flex min-h-full flex-col items-center px-4 pt-[max(3vh,16px)] pb-16">
      <ChatMascot />
      <h1 className="mt-1 text-center text-[28px] leading-tight font-semibold tracking-tight text-foreground sm:text-[32px]">
        {greeting()}. What should we go after?
      </h1>
      <p className="mt-2 mb-6 max-w-md text-center text-[15px] text-muted-foreground">
        Find accounts, reach the right people and act on signals, all from here.
        {stats && stats.total > 0 && (
          <> You have <strong className="font-semibold text-foreground">{stats.total.toLocaleString()}</strong> leads,{" "}
            <strong className="font-semibold text-foreground">{stats.enrichment.with_email.toLocaleString()}</strong> with email.</>
        )}
      </p>

      {composer}

      <section aria-label="Suggestions" className="mt-8 grid w-full max-w-3xl grid-cols-1 gap-2.5 sm:grid-cols-2 lg:grid-cols-3">
        {SUGGESTIONS.map(s => (
          <button key={s.title} type="button" onClick={() => onAction(s.action)} data-tone={s.tone}
            className="gtm-chat-card group flex items-start gap-3 rounded-2xl p-3.5 text-left">
            <span className="gtm-chat-card-icon flex size-9 shrink-0 items-center justify-center rounded-xl">
              <s.icon className="size-[18px]" aria-hidden="true" />
            </span>
            <span className="min-w-0 flex-1">
              <span className="flex items-center gap-1 text-[14px] font-semibold text-foreground">
                {s.title}
                <ArrowUpRight className="size-3.5 opacity-0 transition-opacity group-hover:opacity-60" aria-hidden="true" />
              </span>
              <span className="mt-0.5 block text-[13px] leading-snug text-muted-foreground">{s.body}</span>
            </span>
          </button>
        ))}
      </section>

      {recent.length > 0 && (
        <section aria-label="Recent conversations" className="mt-8 w-full max-w-3xl">
          <h2 className="mb-2 px-1 text-[12px] font-medium tracking-wide text-muted-foreground uppercase">Jump back in</h2>
          <div className="flex flex-col gap-1.5 sm:flex-row">
            {recent.map(c => (
              <Link key={c.id} to={`/chat?id=${c.id}`}
                className="gtm-chat-recent flex min-w-0 flex-1 items-center gap-2 rounded-xl px-3 py-2.5 text-[13px] text-foreground">
                <MessageSquare className="size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
                <span className="truncate">{c.title || "Untitled chat"}</span>
              </Link>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
