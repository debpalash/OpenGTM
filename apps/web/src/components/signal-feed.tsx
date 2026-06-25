// Reusable signal feed row list (spec §4). Contract: `WatchSignal[]` (the full
// shape the watches `/signals` endpoint returns — description/source_url/weight/
// numeric created_at/read 0|1). Extracted from signals.tsx's row loop so the
// Watches slice and the Signals page can share one layout. Optional fields are
// still guarded (`description &&`, `source_url &&`).

import {
  Activity,
  BarChart3,
  Code,
  ExternalLink,
  Globe,
  MessageSquare,
  Newspaper,
  TrendingUp,
  Users,
} from "lucide-react"
import type { WatchSignal } from "@/lib/api"

export const SIGNAL_ICONS: Record<
  string,
  React.ComponentType<{ className?: string }>
> = {
  hiring: Users,
  hiring_surge: Users,
  funding: TrendingUp,
  company_funded: TrendingUp,
  executive_hired: Users,
  tech_change: Code,
  new_tech_adopted: Code,
  website_change: Globe,
  news: Newspaper,
  growth: BarChart3,
  social_activity: MessageSquare,
}

export const SIGNAL_COLORS: Record<string, string> = {
  hiring: "text-blue-400 bg-blue-400/10",
  hiring_surge: "text-blue-400 bg-blue-400/10",
  funding: "text-emerald-400 bg-emerald-400/10",
  company_funded: "text-emerald-400 bg-emerald-400/10",
  executive_hired: "text-violet-400 bg-violet-400/10",
  tech_change: "text-purple-400 bg-purple-400/10",
  new_tech_adopted: "text-purple-400 bg-purple-400/10",
  website_change: "text-amber-400 bg-amber-400/10",
  news: "text-rose-400 bg-rose-400/10",
  growth: "text-sky-400 bg-sky-400/10",
  social_activity: "text-indigo-400 bg-indigo-400/10",
}

export function formatSignalTime(ts: number): string {
  const d = new Date(ts * 1000)
  const diff = Date.now() - d.getTime()
  if (diff < 3600000) return `${Math.floor(diff / 60000)}m ago`
  if (diff < 86400000) return `${Math.floor(diff / 3600000)}h ago`
  return d.toLocaleDateString()
}

export function SignalFeed({ signals }: { signals: WatchSignal[] }) {
  return (
    <div className="space-y-1.5">
      {signals.map((signal) => {
        const Icon = SIGNAL_ICONS[signal.signal_type] || Activity
        const colorClass =
          SIGNAL_COLORS[signal.signal_type] || "text-muted-foreground bg-muted"
        return (
          <div
            key={signal.id}
            className={`flex items-start gap-3 rounded-lg border px-3 py-2.5 transition-colors ${
              signal.read ? "opacity-60" : "border-border/50"
            }`}
          >
            <div
              className={`mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-md ${colorClass}`}
            >
              <Icon className="size-3.5" />
            </div>
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <span className="text-xs font-medium">{signal.title}</span>
                {!signal.read && (
                  <span className="size-1.5 shrink-0 rounded-full bg-blue-400" />
                )}
              </div>
              {signal.description && (
                <p className="mt-0.5 line-clamp-2 text-[11px] text-muted-foreground">
                  {signal.description}
                </p>
              )}
              <div className="mt-1 flex items-center gap-3 text-[10px] text-muted-foreground/60">
                <span>{signal.company}</span>
                <span>{signal.source}</span>
                {signal.source_url && (
                  <a
                    href={signal.source_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="flex items-center gap-0.5 hover:underline"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <ExternalLink className="size-2.5" /> Source
                  </a>
                )}
                <span className="ml-auto">{formatSignalTime(signal.created_at)}</span>
              </div>
            </div>
            <div className="flex shrink-0 items-center gap-1">
              {Array.from({ length: Math.min(5, Math.ceil(signal.weight / 2)) }).map(
                (_, i) => (
                  <span
                    key={i}
                    className={`size-1 rounded-full ${
                      i < Math.ceil(signal.weight / 2) ? "bg-amber-400" : "bg-muted"
                    }`}
                  />
                ),
              )}
            </div>
          </div>
        )
      })}
    </div>
  )
}
