import type { Stats } from "@/lib/api"

export function MetricCards({ stats }: { stats: Stats }) {
  const e = stats.enrichment
  const pct = (n: number) => e.total ? Math.round((n / e.total) * 100) : 0

  const metrics = [
    { label: "Total Leads", value: stats.total, sub: `avg ${Math.round(e.avg_score || 0)} score` },
    { label: "Hot", value: stats.by_tier?.hot ?? 0, color: "text-red-400", bg: "bg-red-500/8", border: "border-red-500/15" },
    { label: "Warm", value: stats.by_tier?.warm ?? 0, color: "text-amber-400", bg: "bg-amber-500/8", border: "border-amber-500/15" },
    { label: "Cold", value: stats.by_tier?.cold ?? 0, color: "text-blue-400", bg: "bg-blue-500/8", border: "border-blue-500/15" },
    { label: "Email", value: `${pct(e.with_email)}%`, sub: `${e.with_email} leads`, color: "text-emerald-400" },
    { label: "Phone", value: `${pct(e.with_phone)}%`, sub: `${e.with_phone} leads`, color: "text-violet-400" },
    { label: "LinkedIn", value: `${pct(e.with_linkedin)}%`, sub: `${e.with_linkedin} leads`, color: "text-sky-400" },
  ]

  return (
    <div className="flex items-stretch gap-0 border-b border-border shrink-0 overflow-x-auto">
      {metrics.map((m, i) => (
        <div
          key={m.label}
          className={`flex-1 min-w-[100px] px-3 py-2 ${i > 0 ? "border-l border-border/50" : ""} ${m.bg || ""} transition-colors hover:bg-accent/30`}
        >
          <div className="text-[10px] text-muted-foreground font-medium uppercase tracking-wider">{m.label}</div>
          <div className={`text-lg font-bold tabular-nums leading-tight ${m.color || "text-foreground"}`}>
            {m.value}
          </div>
          {m.sub && <div className="text-[10px] text-muted-foreground/60">{m.sub}</div>}
        </div>
      ))}
    </div>
  )
}
