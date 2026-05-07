import { useMemo } from "react"
import {
  BarChart3, TrendingUp, Users, Mail, Phone, Globe,
  Link2, Target, Zap, Brain, CheckCircle2,
  ArrowUp, ArrowDown, Activity, Database, Sparkles,
  Layers, CircleDot, Shield,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Skeleton } from "@/components/ui/skeleton"
import {
  useAnalyticsOverview, useAnalyticsPipeline,
  useAnalyticsCollection, useAnalyticsEnrichment, useAnalyticsLLM,
} from "@/lib/hooks"
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip,
  ResponsiveContainer, PieChart, Pie, Cell, AreaChart, Area,

} from "recharts"

// ── Shared ───────────────────────────────────────────────────────
const TIER_COLORS: Record<string, string> = { hot: "#ef4444", warm: "#f59e0b", cold: "#3b82f6", unqualified: "#6b7280" }
const STATUS_COLORS: Record<string, string> = { new: "#8b5cf6", contacted: "#3b82f6", qualified: "#10b981", negotiating: "#f59e0b", converted: "#22c55e", dead: "#6b7280" }
const tt = { contentStyle: { backgroundColor: "var(--color-popover)", border: "1px solid var(--color-border)", borderRadius: "6px", fontSize: "11px", color: "var(--color-foreground)", boxShadow: "0 4px 12px rgba(0,0,0,.15)" }, cursor: { stroke: "var(--color-muted-foreground)", strokeWidth: 1, strokeDasharray: "4 4" } }
const fmt = (n: number) => n >= 1_000_000 ? `${(n / 1_000_000).toFixed(1)}M` : n >= 1_000 ? `${(n / 1_000).toFixed(1)}K` : String(n)

// ── Metric Tile (compact KPI) ────────────────────────────────────
function Metric({ label, value, sub, icon: Icon, color = "text-primary", delta }: {
  label: string; value: string | number; sub?: string; icon: any; color?: string; delta?: number
}) {
  return (
    <div className="group rounded-lg border bg-card p-3 space-y-1 hover:border-primary/20 hover:shadow-sm transition-all duration-200 cursor-default">
      <div className="flex items-center justify-between">
        <span className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider truncate">{label}</span>
        <div className={`p-1.5 rounded-lg bg-gradient-to-br from-primary/10 to-primary/5 ${color} group-hover:scale-110 transition-transform duration-200`}>
          <Icon className="size-3.5" />
        </div>
      </div>
      <div className="flex items-baseline gap-1.5">
        <span className="text-xl font-bold tracking-tight tabular-nums">{value}</span>
        {delta !== undefined && delta !== 0 && (
          <span className={`text-[10px] font-medium flex items-center gap-0.5 ${delta >= 0 ? "text-emerald-500" : "text-red-400"}`}>
            {delta >= 0 ? <ArrowUp className="size-2.5" /> : <ArrowDown className="size-2.5" />}{delta >= 0 ? "+" : ""}{delta}
            <span className="text-muted-foreground/60">wk</span>
          </span>
        )}
      </div>
      {sub && <p className="text-[10px] text-muted-foreground truncate">{sub}</p>}
    </div>
  )
}

// ── Panel wrapper (Grafana-style) ────────────────────────────────
function Panel({ title, icon: Icon, children, className = "", badge }: {
  title: string; icon: any; children: React.ReactNode; className?: string; badge?: string
}) {
  return (
    <div className={`rounded-lg border bg-card overflow-hidden hover:border-primary/15 transition-colors duration-200 ${className}`}>
      <div className="flex items-center justify-between px-3 py-1.5 border-b bg-muted/20">
        <span className="text-[11px] font-medium flex items-center gap-1.5 text-muted-foreground">
          <Icon className="size-3" /> {title}
        </span>
        {badge && <Badge variant="secondary" className="text-[9px] px-1.5 py-0 font-mono tabular-nums">{badge}</Badge>}
      </div>
      <div className="p-3">{children}</div>
    </div>
  )
}

// ── Inline bar (horizontal coverage) ─────────────────────────────
function InlineBar({ label, icon: Icon, count, pct, color = "bg-primary/70" }: {
  label: string; icon: any; count: number; pct: number; color?: string
}) {
  return (
    <div className="flex items-center gap-2 text-[11px]">
      <Icon className="size-3 text-muted-foreground/60 shrink-0" />
      <span className="w-16 text-muted-foreground truncate">{label}</span>
      <div className="flex-1 h-1.5 bg-muted rounded-full overflow-hidden">
        <div className={`h-full rounded-full ${color} transition-all duration-700`} style={{ width: `${Math.min(pct, 100)}%` }} />
      </div>
      <span className="w-8 text-right font-medium tabular-nums">{count}</span>
      <span className="w-8 text-right text-muted-foreground tabular-nums">{pct}%</span>
    </div>
  )
}

// ── Gauge Ring ───────────────────────────────────────────────────
function GaugeRing({ value, max, label, color }: { value: number; max: number; label: string; color: string }) {
  const pct = max > 0 ? Math.round((value / max) * 100) : 0
  const r = 28, sw = 4.5
  const c = 2 * Math.PI * r
  const offset = c - (pct / 100) * c
  return (
    <div className="flex flex-col items-center gap-1 group cursor-default">
      <div className="relative size-16 group-hover:scale-105 transition-transform duration-200">
        <svg viewBox="0 0 66 66" className="size-full -rotate-90" style={{ filter: `drop-shadow(0 0 3px ${color}20)` }}>
          <circle cx="33" cy="33" r={r} fill="none" stroke="currentColor" strokeWidth={sw} className="text-muted/15" />
          <circle cx="33" cy="33" r={r} fill="none" stroke={color} strokeWidth={sw}
            strokeDasharray={c} strokeDashoffset={offset} strokeLinecap="round"
            className="transition-all duration-1000 ease-out" />
        </svg>
        <div className="absolute inset-0 flex flex-col items-center justify-center">
          <span className="text-sm font-bold tabular-nums leading-none">{pct}%</span>
          <span className="text-[8px] text-muted-foreground/50">{value}</span>
        </div>
      </div>
      <span className="text-[10px] text-muted-foreground">{label}</span>
    </div>
  )
}

// ═══════════════════════════════════════════════════════════════
//  ANALYTICS v2 — MAIN PAGE
// ═══════════════════════════════════════════════════════════════

export default function AnalyticsPage() {
  const { data: overview, isLoading: lo } = useAnalyticsOverview()
  const { data: pipeline, isLoading: lp } = useAnalyticsPipeline()
  const { data: collection, isLoading: lc } = useAnalyticsCollection()
  const { data: enrichment, isLoading: le } = useAnalyticsEnrichment()
  const { data: llm, isLoading: ll } = useAnalyticsLLM()
  const isLoading = lo || lp || lc || le || ll

  // ── Derived data ──
  const funnelData = useMemo(() => {
    if (!pipeline?.statuses) return []
    const order = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]
    return order.filter(s => (pipeline.statuses[s] || 0) > 0)
      .map(s => ({ status: s.charAt(0).toUpperCase() + s.slice(1), count: pipeline.statuses[s] || 0, fill: STATUS_COLORS[s] || "#6b7280" }))
  }, [pipeline])

  const tierData = useMemo(() => {
    if (!overview?.tiers) return []
    return Object.entries(overview.tiers).filter(([, v]) => v > 0).map(([k, v]) => ({ name: k, value: v, fill: TIER_COLORS[k] || "#6b7280" }))
  }, [overview])

  const scoreDist = useMemo(() => enrichment?.score_distribution?.map(d => ({ range: d.range, count: d.count })) || [], [enrichment])
  const sourceData = useMemo(() => enrichment?.source_quality?.slice(0, 8).map(s => ({ ...s, source: s.source.replace("job:", "").slice(0, 14) })) || [], [enrichment])

  // Intelligent computed metrics
  const totalLeads = overview?.total_leads || 0
  const enrichRate = overview?.enrichment ? Math.round(((overview.enrichment.with_email + overview.enrichment.with_phone + overview.enrichment.with_website) / (overview.enrichment.total * 3 || 1)) * 100) : 0
  const dataQuality = overview?.enrichment ? Math.round(((overview.enrichment.with_email + overview.enrichment.with_contact + overview.enrichment.with_linkedin) / (overview.enrichment.total * 3 || 1)) * 100) : 0
  const conversionRate = pipeline?.statuses ? Math.round(((pipeline.statuses["qualified"] || 0) + (pipeline.statuses["converted"] || 0)) / (totalLeads || 1) * 100) : 0
  const avgLeadsPerJob = collection?.avg_leads_per_job || 0

  if (isLoading) return (
    <div className="p-4 space-y-3">
      <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-6 gap-2">
        {[...Array(6)].map((_, i) => <Skeleton key={i} className="h-20 rounded-lg" />)}
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-2">
        {[...Array(3)].map((_, i) => <Skeleton key={i} className="h-52 rounded-lg" />)}
      </div>
    </div>
  )

  return (
    <div className="h-full overflow-y-auto">
      <div className="p-4 space-y-3 max-w-[1600px] mx-auto">

        {/* ── Row 0: Header strip ── */}
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <BarChart3 className="size-4 text-primary" />
            <h2 className="text-sm font-semibold">Analytics Dashboard</h2>
            <Badge variant="outline" className="text-[9px] font-mono">{totalLeads} leads</Badge>
          </div>
          <div className="flex items-center gap-2 text-[10px] text-muted-foreground">
            <CircleDot className="size-2.5 text-emerald-500 fill-emerald-500" />
            Live · {new Date().toLocaleDateString("en-US", { month: "short", day: "numeric" })}
          </div>
        </div>

        {/* ── Row 1: KPI Metrics (6 tiles) ── */}
        <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-2">
          <Metric label="Total Leads" value={fmt(totalLeads)} icon={Database} delta={overview?.leads_this_week} sub={`${overview?.leads_this_month || 0} this month`} />
          <Metric label="Avg Score" value={overview?.avg_score || 0} icon={Target} color="text-amber-500" sub={`${overview?.tiers?.hot || 0} hot · ${overview?.tiers?.warm || 0} warm`} />
          <Metric label="Email Rate" value={`${overview?.enrichment?.email_pct || 0}%`} icon={Mail} color="text-emerald-500" sub={`${overview?.enrichment?.with_email || 0} verified`} />
          <Metric label="Conversion" value={`${conversionRate}%`} icon={TrendingUp} color="text-violet-500" sub={`${pipeline?.statuses?.qualified || 0} qualified`} />
          <Metric label="Data Quality" value={`${dataQuality}%`} icon={Shield} color="text-cyan-500" sub="email+contact+linkedin" />
          <Metric label="Job Success" value={`${overview?.jobs?.success_rate || 0}%`} icon={Zap} color="text-orange-500" sub={`${overview?.jobs?.completed || 0}/${overview?.jobs?.total || 0} done`} />
        </div>

        {/* ── Row 2: Coverage Gauges + Pipeline + Tiers ── */}
        <div className="grid grid-cols-1 lg:grid-cols-12 gap-2">

          {/* Enrichment Coverage Gauges */}
          <Panel title="Enrichment Coverage" icon={CheckCircle2} className="lg:col-span-3" badge={`${enrichRate}% avg`}>
            {overview?.enrichment ? (
              <div className="space-y-3">
                <div className="grid grid-cols-3 gap-1">
                  <GaugeRing value={overview.enrichment.with_email} max={overview.enrichment.total} label="Email" color="#10b981" />
                  <GaugeRing value={overview.enrichment.with_phone} max={overview.enrichment.total} label="Phone" color="#3b82f6" />
                  <GaugeRing value={overview.enrichment.with_website} max={overview.enrichment.total} label="Website" color="#8b5cf6" />
                </div>
                <div className="space-y-1.5">
                  <InlineBar label="Email" icon={Mail} count={overview.enrichment.with_email} pct={overview.enrichment.email_pct} color="bg-emerald-500/70" />
                  <InlineBar label="Phone" icon={Phone} count={overview.enrichment.with_phone} pct={overview.enrichment.phone_pct} color="bg-blue-500/70" />
                  <InlineBar label="Website" icon={Globe} count={overview.enrichment.with_website} pct={overview.enrichment.website_pct} color="bg-violet-500/70" />
                  <InlineBar label="Contact" icon={Users} count={overview.enrichment.with_contact} pct={overview.enrichment.contact_pct} color="bg-amber-500/70" />
                  <InlineBar label="LinkedIn" icon={Link2} count={overview.enrichment.with_linkedin} pct={Math.round((overview.enrichment.with_linkedin / (overview.enrichment.total || 1)) * 100)} color="bg-cyan-500/70" />
                </div>
                {overview.email_confidence && Object.keys(overview.email_confidence).length > 0 && (
                  <div className="flex flex-wrap gap-1 pt-1 border-t">
                    {Object.entries(overview.email_confidence).map(([k, v]) => (
                      <Badge key={k} variant={k === "smtp_verified" ? "default" : "secondary"} className="text-[9px] px-1 py-0">
                        {k}: {v}
                      </Badge>
                    ))}
                  </div>
                )}
              </div>
            ) : <Empty />}
          </Panel>

          {/* Pipeline Funnel */}
          <Panel title="Pipeline Funnel" icon={Activity} className="lg:col-span-5" badge={`${funnelData.length} stages`}>
            {funnelData.length > 0 ? (
              <ResponsiveContainer width="100%" height={200}>
                <BarChart data={funnelData} layout="vertical" margin={{ left: 10, right: 10 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis type="number" tick={{ fontSize: 10, fill: "var(--color-muted-foreground)" }} />
                  <YAxis dataKey="status" type="category" width={80} tick={{ fontSize: 10, fill: "var(--color-muted-foreground)" }} />
                  <Tooltip {...tt} />
                  <Bar dataKey="count" radius={[0, 4, 4, 0]}>
                    {funnelData.map((e, i) => <Cell key={i} fill={e.fill} />)}
                  </Bar>
                </BarChart>
              </ResponsiveContainer>
            ) : <Empty />}
          </Panel>

          {/* Tier Distribution */}
          <Panel title="Score Tiers" icon={Sparkles} className="lg:col-span-4">
            {tierData.length > 0 ? (
              <div className="flex items-center gap-4">
                <ResponsiveContainer width="50%" height={160}>
                  <PieChart>
                    <Pie data={tierData} cx="50%" cy="50%" innerRadius={35} outerRadius={55} paddingAngle={3} dataKey="value">
                      {tierData.map((e, i) => <Cell key={i} fill={e.fill} />)}
                    </Pie>
                    <Tooltip {...tt} />
                  </PieChart>
                </ResponsiveContainer>
                <div className="space-y-2 flex-1">
                  {tierData.map(t => {
                    const pct = Math.round((t.value / totalLeads) * 100)
                    return (
                      <div key={t.name} className="space-y-0.5">
                        <div className="flex items-center justify-between text-[11px]">
                          <div className="flex items-center gap-1.5">
                            <div className="size-2 rounded-full" style={{ backgroundColor: t.fill }} />
                            <span className="capitalize text-muted-foreground">{t.name}</span>
                          </div>
                          <span className="font-medium tabular-nums">{t.value} <span className="text-muted-foreground">({pct}%)</span></span>
                        </div>
                        <div className="h-1 bg-muted rounded-full overflow-hidden">
                          <div className="h-full rounded-full transition-all duration-500" style={{ width: `${pct}%`, backgroundColor: t.fill }} />
                        </div>
                      </div>
                    )
                  })}
                </div>
              </div>
            ) : <Empty />}
          </Panel>
        </div>

        {/* ── Row 3: Trends (leads + scores) ── */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-2">
          <Panel title="Lead Collection (30d)" icon={TrendingUp} badge={collection?.leads_by_day ? `${collection.leads_by_day.reduce((s, d) => s + d.count, 0)} total` : undefined}>
            {collection?.leads_by_day?.length ? (
              <ResponsiveContainer width="100%" height={160}>
                <AreaChart data={collection.leads_by_day}>
                  <defs><linearGradient id="gl" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="#8b5cf6" stopOpacity={0.3} /><stop offset="95%" stopColor="#8b5cf6" stopOpacity={0} /></linearGradient></defs>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis dataKey="day" tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: string) => v.slice(5)} />
                  <YAxis tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} />
                  <Tooltip {...tt} />
                  <Area type="monotone" dataKey="count" stroke="#8b5cf6" fillOpacity={1} fill="url(#gl)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            ) : <Empty />}
          </Panel>

          <Panel title="Score Distribution" icon={Target}>
            {scoreDist.length > 0 ? (
              <ResponsiveContainer width="100%" height={160}>
                <BarChart data={scoreDist}>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis dataKey="range" tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} />
                  <YAxis tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} />
                  <Tooltip {...tt} />
                  <Bar dataKey="count" fill="#10b981" radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            ) : <Empty />}
          </Panel>
        </div>

        {/* ── Row 4: Cities + Source Quality ── */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-2">
          <Panel title="Top Cities" icon={Globe} badge={enrichment?.by_city ? `${enrichment.by_city.length} cities` : undefined}>
            {enrichment?.by_city?.length ? (
              <div className="space-y-1.5 max-h-[200px] overflow-y-auto">
                {enrichment.by_city.slice(0, 12).map((c, i) => (
                  <div key={c.city} className="flex items-center gap-2 text-[11px]">
                    <span className="w-3 text-muted-foreground/40 tabular-nums">{i + 1}</span>
                    <span className="w-24 truncate text-muted-foreground">{c.city}</span>
                    <div className="flex-1 h-1.5 bg-muted rounded-full overflow-hidden">
                      <div className="h-full rounded-full bg-blue-500/60 transition-all duration-500"
                        style={{ width: `${(c.count / (enrichment.by_city[0]?.count || 1)) * 100}%` }} />
                    </div>
                    <span className="w-7 text-right font-medium tabular-nums">{c.count}</span>
                    <Badge variant="secondary" className="text-[9px] px-1 py-0 w-10 justify-center tabular-nums">
                      {c.avg_score}
                    </Badge>
                  </div>
                ))}
              </div>
            ) : <Empty />}
          </Panel>

          <Panel title="Source Quality" icon={Layers} badge={sourceData.length ? `${sourceData.length} sources` : undefined}>
            {sourceData.length > 0 ? (
              <div className="space-y-1.5 max-h-[200px] overflow-y-auto">
                {sourceData.map((s, i) => (
                  <div key={s.source} className="flex items-center gap-2 text-[11px]">
                    <span className="w-3 text-muted-foreground/40 tabular-nums">{i + 1}</span>
                    <span className="w-28 truncate font-mono text-muted-foreground">{s.source}</span>
                    <div className="flex-1 h-1.5 bg-muted rounded-full overflow-hidden">
                      <div className="h-full rounded-full bg-amber-500/60 transition-all duration-500"
                        style={{ width: `${(s.count / (sourceData[0]?.count || 1)) * 100}%` }} />
                    </div>
                    <span className="w-7 text-right font-medium tabular-nums">{s.count}</span>
                    <Badge variant="secondary" className="text-[9px] px-1 py-0 w-10 justify-center tabular-nums">
                      {s.avg_score}
                    </Badge>
                  </div>
                ))}
              </div>
            ) : <Empty />}
          </Panel>
        </div>

        {/* ── Row 5: LLM Intelligence ── */}
        <div className="grid grid-cols-1 lg:grid-cols-12 gap-2">
          <Panel title="LLM Token Usage (30d)" icon={Brain} className="lg:col-span-8"
            badge={llm?.total_tokens ? `${fmt(llm.total_tokens)} tokens · ${llm.total_calls} calls` : undefined}>
            {llm?.by_day?.length ? (
              <ResponsiveContainer width="100%" height={140}>
                <AreaChart data={llm.by_day}>
                  <defs><linearGradient id="gt" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="#06b6d4" stopOpacity={0.3} /><stop offset="95%" stopColor="#06b6d4" stopOpacity={0} /></linearGradient></defs>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis dataKey="day" tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: string) => v.slice(5)} />
                  <YAxis tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: number) => fmt(v)} />
                  <Tooltip {...tt} />
                  <Area type="monotone" dataKey="tokens" stroke="#06b6d4" fillOpacity={1} fill="url(#gt)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            ) : <Empty />}
          </Panel>

          <Panel title="LLM Providers" icon={Zap} className="lg:col-span-4">
            {llm?.by_provider?.length ? (
              <div className="space-y-2">
                {llm.by_provider.map(p => (
                  <div key={p.provider} className="flex items-center gap-2 text-[11px]">
                    <span className="flex-1 font-mono truncate text-muted-foreground">{p.provider}</span>
                    <span className="font-medium tabular-nums">{fmt(p.tokens || 0)}</span>
                    <Badge variant="secondary" className="text-[9px] px-1 py-0 tabular-nums">{p.calls}c</Badge>
                  </div>
                ))}
              </div>
            ) : <Empty />}
          </Panel>
        </div>

        {/* ── Row 6: Collection Jobs Trend ── */}
        {collection?.jobs_by_day && collection.jobs_by_day.length > 0 && (
          <Panel title="Jobs vs Leads (30d)" icon={Activity} badge={`avg ${avgLeadsPerJob.toFixed(1)} leads/job`}>
            <ResponsiveContainer width="100%" height={140}>
              <BarChart data={collection.jobs_by_day}>
                <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                <XAxis dataKey="day" tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: string) => v.slice(5)} />
                <YAxis tick={{ fontSize: 9, fill: "var(--color-muted-foreground)" }} />
                <Tooltip {...tt} />
                <Bar dataKey="jobs" fill="#8b5cf6" radius={[3, 3, 0, 0]} />
                <Bar dataKey="leads" fill="#10b981" radius={[3, 3, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </Panel>
        )}

      </div>
    </div>
  )
}

function Empty() {
  return (
    <div className="h-20 flex flex-col items-center justify-center gap-1">
      <Database className="size-4 text-muted-foreground/20" />
      <span className="text-[10px] text-muted-foreground/40">No data available</span>
    </div>
  )
}
