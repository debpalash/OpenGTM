import { useMemo, type ReactNode } from "react"
import type { UseQueryResult } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { ErrorState, PageHeader } from "@/design-system/patterns/workbench"
import { relativeTime } from "@/lib/record-overview"
import { analyticsFreshness, analyticsRegionState } from "@/lib/analytics-state"
import {
  TrendingUp, Users, Mail, Phone, Globe,
  Link2, Target, Zap, Brain, CheckCircle2,
  ArrowUp, ArrowDown, Activity, Database, Sparkles,
  Layers, Shield,
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
const TIER_COLORS: Record<string, string> = { hot: "var(--gtm-danger)", warm: "var(--gtm-warning)", cold: "var(--gtm-link)", unqualified: "var(--muted-foreground)" }
const STATUS_COLORS: Record<string, string> = { new: "var(--gtm-chart-purple)", contacted: "var(--gtm-link)", qualified: "var(--gtm-success)", negotiating: "var(--gtm-warning)", converted: "var(--gtm-success)", dead: "var(--muted-foreground)" }
const tt = { contentStyle: { backgroundColor: "var(--color-popover)", border: "1px solid var(--color-border)", borderRadius: "6px", fontSize: "11px", color: "var(--color-foreground)", boxShadow: "0 4px 12px rgba(0,0,0,.15)" }, cursor: { stroke: "var(--color-muted-foreground)", strokeWidth: 1, strokeDasharray: "4 4" } }
const fmt = (n: number) => n >= 1_000_000 ? `${(n / 1_000_000).toFixed(1)}M` : n >= 1_000 ? `${(n / 1_000).toFixed(1)}K` : String(n)

// ── Metric Tile (compact KPI) ────────────────────────────────────
function Metric({ label, value, sub, icon: Icon, color = "text-primary", delta }: {
  label: string; value: string | number; sub?: string; icon: any; color?: string; delta?: number
}) {
  return (
    <div className="group rounded-lg border bg-card p-3 space-y-1 hover:border-primary/20 hover:shadow-sm transition-all duration-200 cursor-default">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider truncate">{label}</span>
        <div className={`p-1.5 rounded-lg bg-gradient-to-br from-primary/10 to-primary/5 ${color} group-hover:scale-110 transition-transform duration-200`}>
          <Icon className="size-3.5" />
        </div>
      </div>
      <div className="flex items-baseline gap-1.5">
        <span className="text-xl font-bold tracking-tight tabular-nums">{value}</span>
        {delta !== undefined && delta !== 0 && (
          <span className={`text-xs font-medium flex items-center gap-0.5 ${delta >= 0 ? "text-emerald-700 dark:text-emerald-400" : "text-red-700 dark:text-red-400"}`}>
            {delta >= 0 ? <ArrowUp className="size-2.5" /> : <ArrowDown className="size-2.5" />}{delta >= 0 ? "+" : ""}{delta}
            <span className="text-muted-foreground">wk</span>
          </span>
        )}
      </div>
      {sub && <p className="text-xs text-muted-foreground truncate">{sub}</p>}
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
        <span className="text-xs font-medium flex items-center gap-1.5 text-muted-foreground">
          <Icon className="size-3" /> {title}
        </span>
        {badge && <Badge variant="secondary" className="text-xs px-1.5 py-0 font-mono tabular-nums">{badge}</Badge>}
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
    <div className="flex items-center gap-2 text-xs">
      <Icon className="size-3 text-muted-foreground shrink-0" />
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
          <span className="text-xs text-muted-foreground">{value}</span>
        </div>
      </div>
      <span className="text-xs text-muted-foreground">{label}</span>
    </div>
  )
}

// ═══════════════════════════════════════════════════════════════
//  ANALYTICS v2 — MAIN PAGE
// ═══════════════════════════════════════════════════════════════

export default function AnalyticsPage() {
  const overviewQuery = useAnalyticsOverview()
  const { data: overview } = overviewQuery
  const pipelineQuery = useAnalyticsPipeline()
  const { data: pipeline } = pipelineQuery
  const collectionQuery = useAnalyticsCollection()
  const { data: collection } = collectionQuery
  const enrichmentQuery = useAnalyticsEnrichment()
  const { data: enrichment } = enrichmentQuery
  const llmQuery = useAnalyticsLLM()
  const { data: llm } = llmQuery
  const requests = [overviewQuery, pipelineQuery, collectionQuery, enrichmentQuery, llmQuery]
  const allLoading = requests.every(query => query.data === undefined && !query.isError)
  const allUnavailable = requests.every(query => query.data === undefined && query.isError)
  const staleCount = requests.filter(query => query.data !== undefined && query.isError).length
  const lastLoaded = analyticsFreshness(requests)
  const retry = () => { for (const query of requests) void query.refetch() }
  const retryFailed = () => { for (const query of requests) if (query.isError) void query.refetch() }

  // ── Derived data ──
  const funnelData = useMemo(() => {
    if (!pipeline?.statuses) return []
    const order = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]
    return order.filter(s => (pipeline.statuses[s] || 0) > 0)
      .map(s => ({ status: s.charAt(0).toUpperCase() + s.slice(1), count: pipeline.statuses[s] || 0, fill: STATUS_COLORS[s] || "var(--muted-foreground)" }))
  }, [pipeline])

  const tierData = useMemo(() => {
    if (!overview?.tiers) return []
    return Object.entries(overview.tiers).filter(([, v]) => v > 0).map(([k, v]) => ({ name: k, value: v, fill: TIER_COLORS[k] || "var(--muted-foreground)" }))
  }, [overview])

  const scoreDist = useMemo(() => enrichment?.score_distribution?.map(d => ({ range: d.range, count: d.count })) || [], [enrichment])
  const sourceData = useMemo(() => enrichment?.source_quality?.slice(0, 8).map(s => ({ ...s, source: s.source.replace("job:", "").slice(0, 14) })) || [], [enrichment])

  // Intelligent computed metrics
  const totalLeads = overview?.total_leads || 0
  const enrichRate = overview?.enrichment ? Math.round(((overview.enrichment.with_email + overview.enrichment.with_phone + overview.enrichment.with_website) / (overview.enrichment.total * 3 || 1)) * 100) : 0
  const dataQuality = overview?.enrichment ? Math.round(((overview.enrichment.with_email + overview.enrichment.with_contact + overview.enrichment.with_linkedin) / (overview.enrichment.total * 3 || 1)) * 100) : 0
  const conversionRate = pipeline?.statuses ? Math.round(((pipeline.statuses["qualified"] || 0) + (pipeline.statuses["converted"] || 0)) / (totalLeads || 1) * 100) : 0
  const avgLeadsPerJob = collection?.avg_leads_per_job || 0
  // Avoid a misleading "0%" when a real (but tiny vs the total) count exists.
  const softPct = (pct: number, count: number) => (pct <= 0 && count > 0 ? "<1%" : `${pct}%`)
  const qualifiedCount = (pipeline?.statuses?.["qualified"] || 0) + (pipeline?.statuses?.["converted"] || 0)

  if (allUnavailable) return <div className="workbench-page"><PageHeader title="Analytics" description="Historical lead pipeline and enrichment performance." /><ErrorState title="Analytics is temporarily unavailable" error={requests.find(query => query.isError)?.error} retry={retry} /></div>
  if (allLoading) return (
    <div className="p-3 space-y-3" role="status" aria-label="Loading analytics">
      <PageHeader title="Analytics" description="Historical lead pipeline and enrichment performance." />
      <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-6 gap-2">
        {[...Array(6)].map((_, i) => <Skeleton key={i} className="h-20 rounded-lg" />)}
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-2">
        {[...Array(3)].map((_, i) => <Skeleton key={i} className="h-52 rounded-lg" />)}
      </div>
      <span className="sr-only">Loading analytics…</span>
    </div>
  )

  return (
    <div className="h-full overflow-y-auto">
      <div className="p-3 space-y-3 max-w-[1600px] mx-auto">

        <PageHeader title="Analytics" description="Historical lead pipeline and enrichment performance." actions={<Link to="/overview" className="workbench-text-action">Company & people overview</Link>} />
        {staleCount > 0 && <div className="overview-notice" role="alert">The latest refresh failed for {staleCount === 1 ? "one section" : `${staleCount} sections`}. Showing the last successful data, which may be out of date. <button type="button" onClick={retryFailed}>Retry</button></div>}
        <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground"><span>Scope: legacy leads</span><span>{lastLoaded === null ? "Not loaded yet" : `Updated ${relativeTime(new Date(lastLoaded).toISOString()).toLowerCase()}`}</span></div>

        <details className="analytics-data-tables"><summary>View chart data as tables</summary><div>
          <AnalyticsTable title="Pipeline stages" columns={["Stage","Leads"]} rows={funnelData.map(row=>[row.status,row.count])} query={pipelineQuery}/>
          <AnalyticsTable title="Score tiers" columns={["Tier","Leads"]} rows={tierData.map(row=>[row.name,row.value])} query={overviewQuery}/>
          <AnalyticsTable title="Leads collected by day" columns={["Day","Leads"]} rows={collection?.leads_by_day.map(row=>[row.day,row.count])??[]} query={collectionQuery}/>
          <AnalyticsTable title="Score distribution" columns={["Range","Leads"]} rows={scoreDist.map(row=>[row.range,row.count])} query={enrichmentQuery}/>
          <AnalyticsTable title="Jobs and leads by day" columns={["Day","Jobs","Leads","Completed jobs"]} rows={collection?.jobs_by_day.map(row=>[row.day,row.jobs,row.leads,row.completed])??[]} query={collectionQuery}/>
          <AnalyticsTable title="LLM usage by day" columns={["Day","Tokens","Calls","Providers"]} rows={llm?.by_day.map(row=>[row.day,row.tokens,row.calls,row.providers])??[]} query={llmQuery}/>
        </div></details>
        {/* ── Row 1: KPI Metrics (6 tiles) ── */}
        <Region query={overviewQuery} label="Summary metrics" emptyHint="" loadingHeight="h-20">{() => (
        <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-2">
          <Metric label="Total Leads" value={fmt(totalLeads)} icon={Database} delta={overview?.leads_this_week} sub={`${overview?.leads_this_month || 0} this month`} />
          <Metric label="Avg Score" value={overview?.avg_score || 0} icon={Target} color="text-amber-500" sub={`${overview?.tiers?.hot || 0} hot · ${overview?.tiers?.warm || 0} warm`} />
          <Metric label="Email Rate" value={softPct(overview?.enrichment?.email_pct || 0, overview?.enrichment?.with_email || 0)} icon={Mail} color="text-emerald-500" sub={`${fmt(overview?.enrichment?.with_email || 0)} verified`} />
          <Metric label="Conversion" value={pipeline ? softPct(conversionRate, qualifiedCount) : "—"} icon={TrendingUp} color="text-violet-500" sub={pipeline ? `${fmt(qualifiedCount)} qualified` : pipelineQuery.isError ? "Pipeline unavailable" : "Loading pipeline"} />
          <Metric label="Data Quality" value={softPct(dataQuality, overview?.enrichment?.with_contact || 0)} icon={Shield} color="text-cyan-500" sub="email+contact+linkedin" />
          <Metric label="Job Success" value={`${overview?.jobs?.success_rate || 0}%`} icon={Zap} color="text-orange-500" sub={`${overview?.jobs?.completed || 0}/${overview?.jobs?.total || 0} done`} />
        </div>
        )}</Region>

        {/* ── Row 2: Coverage Gauges + Pipeline + Tiers ── */}
        <div className="grid grid-cols-1 lg:grid-cols-12 gap-2">

          {/* Enrichment Coverage Gauges */}
          <Panel title="Enrichment Coverage" icon={CheckCircle2} className="lg:col-span-3" badge={`${enrichRate}% avg`}>
            <Region query={overviewQuery} label="Enrichment coverage" isEmpty={o => o.total_leads === 0} emptyHint="No leads yet. Coverage appears once leads are collected." loadingHeight="h-40">{(overview) => (
              <div className="space-y-3">
                <div className="grid grid-cols-3 gap-1">
                  <GaugeRing value={overview.enrichment.with_email} max={overview.enrichment.total} label="Email" color="var(--gtm-success)" />
                  <GaugeRing value={overview.enrichment.with_phone} max={overview.enrichment.total} label="Phone" color="var(--gtm-link)" />
                  <GaugeRing value={overview.enrichment.with_website} max={overview.enrichment.total} label="Website" color="var(--gtm-chart-purple)" />
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
                      <Badge key={k} variant={k === "smtp_verified" ? "default" : "secondary"} className="text-xs px-1 py-0">
                        {k}: {v}
                      </Badge>
                    ))}
                  </div>
                )}
              </div>
            )}</Region>
          </Panel>

          {/* Pipeline Funnel */}
          <Panel title="Pipeline Funnel" icon={Activity} className="lg:col-span-5" badge={`${funnelData.length} stages`}>
            <Region query={pipelineQuery} label="Pipeline funnel" isEmpty={p => !Object.values(p.statuses).some(v => v > 0)} emptyHint="No leads in the pipeline yet. Stages fill in as leads are collected." loadingHeight="h-40">{() => (
              <ResponsiveContainer width="100%" height={200}>
                <BarChart data={funnelData} layout="vertical" margin={{ left: 10, right: 10 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis type="number" tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} />
                  <YAxis dataKey="status" type="category" width={80} tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} />
                  <Tooltip {...tt} />
                  <Bar dataKey="count" radius={[0, 4, 4, 0]}>
                    {funnelData.map((e, i) => <Cell key={i} fill={e.fill} />)}
                  </Bar>
                </BarChart>
              </ResponsiveContainer>
            )}</Region>
          </Panel>

          {/* Tier Distribution */}
          <Panel title="Score Tiers" icon={Sparkles} className="lg:col-span-4">
            <Region query={overviewQuery} label="Score tiers" isEmpty={o => !Object.values(o.tiers).some(v => v > 0)} emptyHint="No scored leads yet. Tiers fill in once leads are scored." loadingHeight="h-40">{() => (
              <div className="flex items-center gap-3">
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
                        <div className="flex items-center justify-between text-xs">
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
            )}</Region>
          </Panel>
        </div>

        {/* ── Row 3: Trends (leads + scores) ── */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-2">
          <Panel title="Lead Collection (30d)" icon={TrendingUp} badge={collection?.leads_by_day ? `${collection.leads_by_day.reduce((s, d) => s + d.count, 0)} total` : undefined}>
            <Region query={collectionQuery} label="Lead collection" isEmpty={c => c.leads_by_day.length === 0} emptyHint="No leads collected in the last 30 days. Run a collection job to fill this." loadingHeight="h-40">{(collection) => (
              <ResponsiveContainer width="100%" height={160}>
                <AreaChart data={collection.leads_by_day}>
                  <defs><linearGradient id="gl" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="var(--gtm-chart-purple)" stopOpacity={0.3} /><stop offset="95%" stopColor="var(--gtm-chart-purple)" stopOpacity={0} /></linearGradient></defs>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis dataKey="day" tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: string) => v.slice(5)} />
                  <YAxis tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} />
                  <Tooltip {...tt} />
                  <Area type="monotone" dataKey="count" stroke="var(--gtm-chart-purple)" fillOpacity={1} fill="url(#gl)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            )}</Region>
          </Panel>

          <Panel title="Score Distribution" icon={Target}>
            <Region query={enrichmentQuery} label="Score distribution" isEmpty={e => e.score_distribution.length === 0} emptyHint="No scores yet. This fills in once leads are enriched and scored." loadingHeight="h-40">{() => (
              <ResponsiveContainer width="100%" height={160}>
                <BarChart data={scoreDist}>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis dataKey="range" tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} />
                  <YAxis tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} />
                  <Tooltip {...tt} />
                  <Bar dataKey="count" fill="var(--gtm-success)" radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            )}</Region>
          </Panel>
        </div>

        {/* ── Row 4: Cities + Source Quality ── */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-2">
          <Panel title="Top Cities" icon={Globe} badge={enrichment?.by_city ? `${enrichment.by_city.length} cities` : undefined}>
            <Region query={enrichmentQuery} label="Top cities" isEmpty={e => e.by_city.length === 0} emptyHint="No city data yet. Cities appear once collected leads include a location.">{(enrichment) => (
              <div className="space-y-1.5 max-h-[200px] overflow-y-auto">
                {enrichment.by_city.slice(0, 12).map((c, i) => (
                  <div key={c.city} className="flex items-center gap-2 text-xs">
                    <span className="w-3 text-muted-foreground tabular-nums">{i + 1}</span>
                    <span className="w-24 truncate text-muted-foreground">{c.city || "Unknown city"}</span>
                    <div className="flex-1 h-1.5 bg-muted rounded-full overflow-hidden">
                      <div className="h-full rounded-full bg-blue-500/60 transition-all duration-500"
                        style={{ width: `${(c.count / (enrichment.by_city[0]?.count || 1)) * 100}%` }} />
                    </div>
                    <span className="w-7 text-right font-medium tabular-nums">{c.count}</span>
                    <Badge variant="secondary" className="text-xs px-1 py-0 w-10 justify-center tabular-nums">
                      {c.avg_score ?? "—"}
                    </Badge>
                  </div>
                ))}
              </div>
            )}</Region>
          </Panel>

          <Panel title="Source Quality" icon={Layers} badge={sourceData.length ? `${sourceData.length} sources` : undefined}>
            <Region query={enrichmentQuery} label="Source quality" isEmpty={e => e.source_quality.length === 0} emptyHint="No source data yet. This fills in as jobs collect leads.">{() => (
              <div className="space-y-1.5 max-h-[200px] overflow-y-auto">
                {sourceData.map((s, i) => (
                  <div key={s.source} className="flex items-center gap-2 text-xs">
                    <span className="w-3 text-muted-foreground tabular-nums">{i + 1}</span>
                    <span className="w-28 truncate font-mono text-muted-foreground">{s.source}</span>
                    <div className="flex-1 h-1.5 bg-muted rounded-full overflow-hidden">
                      <div className="h-full rounded-full bg-amber-500/60 transition-all duration-500"
                        style={{ width: `${(s.count / (sourceData[0]?.count || 1)) * 100}%` }} />
                    </div>
                    <span className="w-7 text-right font-medium tabular-nums">{s.count}</span>
                    <Badge variant="secondary" className="text-xs px-1 py-0 w-10 justify-center tabular-nums">
                      {s.avg_score ?? "—"}
                    </Badge>
                  </div>
                ))}
              </div>
            )}</Region>
          </Panel>
        </div>

        {/* ── Row 5: LLM Intelligence ── */}
        <div className="grid grid-cols-1 lg:grid-cols-12 gap-2">
          <Panel title="LLM Token Usage (30d)" icon={Brain} className="lg:col-span-8"
            badge={llm?.total_tokens ? `${fmt(llm.total_tokens)} tokens · ${llm.total_calls} calls` : undefined}>
            <Region query={llmQuery} label="LLM token usage" isEmpty={l => l.by_day.length === 0} emptyHint="No LLM calls in the last 30 days. Usage appears when AI columns or enrichment run." loadingHeight="h-40">{(llm) => (
              <ResponsiveContainer width="100%" height={140}>
                <AreaChart data={llm.by_day}>
                  <defs><linearGradient id="gt" x1="0" y1="0" x2="0" y2="1"><stop offset="5%" stopColor="var(--gtm-chart-teal)" stopOpacity={0.3} /><stop offset="95%" stopColor="var(--gtm-chart-teal)" stopOpacity={0} /></linearGradient></defs>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                  <XAxis dataKey="day" tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: string) => v.slice(5)} />
                  <YAxis tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: number) => fmt(v)} />
                  <Tooltip {...tt} />
                  <Area type="monotone" dataKey="tokens" stroke="var(--gtm-chart-teal)" fillOpacity={1} fill="url(#gt)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            )}</Region>
          </Panel>

          <Panel title="LLM Providers" icon={Zap} className="lg:col-span-4">
            <Region query={llmQuery} label="LLM providers" isEmpty={l => l.by_provider.length === 0} emptyHint="No provider usage yet.">{(llm) => (
              <div className="space-y-2">
                {llm.by_provider.map(p => (
                  <div key={p.provider} className="flex items-center gap-2 text-xs">
                    <span className="flex-1 font-mono truncate text-muted-foreground">{p.provider}</span>
                    <span className="font-medium tabular-nums">{fmt(p.tokens || 0)}</span>
                    <Badge variant="secondary" className="text-xs px-1 py-0 tabular-nums">{p.calls}c</Badge>
                  </div>
                ))}
              </div>
            )}</Region>
          </Panel>
        </div>

        {/* ── Row 6: Collection Jobs Trend ── */}
          <Panel title="Jobs vs Leads (30d)" icon={Activity} badge={collection?.jobs_by_day?.length ? `avg ${avgLeadsPerJob.toFixed(1)} leads/job` : undefined}>
            <Region query={collectionQuery} label="Jobs and leads" isEmpty={c => c.jobs_by_day.length === 0} emptyHint="No collection jobs in the last 30 days. Run a job to fill this.">{(collection) => (
              <ResponsiveContainer width="100%" height={140}>
              <BarChart data={collection.jobs_by_day}>
                <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
                <XAxis dataKey="day" tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} tickFormatter={(v: string) => v.slice(5)} />
                <YAxis tick={{ fontSize: 12, fill: "var(--color-muted-foreground)" }} />
                <Tooltip {...tt} />
                <Bar dataKey="jobs" fill="var(--gtm-chart-purple)" radius={[3, 3, 0, 0]} />
                <Bar dataKey="leads" fill="var(--gtm-success)" radius={[3, 3, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
            )}</Region>
          </Panel>

      </div>
    </div>
  )
}

function Empty({ hint = "No data available" }: { hint?: string }) {
  return (
    <div className="h-20 flex flex-col items-center justify-center gap-1 text-center">
      <Database className="size-4 text-muted-foreground/20" aria-hidden="true" />
      <span className="text-xs text-muted-foreground">{hint}</span>
    </div>
  )
}

/** One analytics data region: loading, error (with Retry), empty, or loaded. */
function Region<T>({ query, label, isEmpty, emptyHint, loadingHeight = "h-20", children }: {
  query: UseQueryResult<T>
  label: string
  isEmpty?: (data: T) => boolean
  emptyHint: string
  loadingHeight?: string
  children: (data: T) => ReactNode
}) {
  const state = analyticsRegionState(query, isEmpty)
  if (state.kind === "loading") return (
    <div role="status" aria-label={`Loading ${label}`}>
      <Skeleton className={`${loadingHeight} rounded-lg`} />
      <span className="sr-only">Loading {label}…</span>
    </div>
  )
  if (state.kind === "error") return (
    <ErrorState title={`${label} is unavailable`} error={query.error} retry={() => void query.refetch()} />
  )
  if (state.kind === "empty") return <Empty hint={emptyHint} />
  return <>{children(query.data as T)}</>
}

function AnalyticsTable({title,columns,rows,query}:{title:string;columns:string[];rows:(string|number)[][];query:{data:unknown;isError:boolean}}) { return <section><h2>{title}</h2>{rows.length?<table><caption className="sr-only">{title}</caption><thead><tr>{columns.map(column=><th scope="col" key={column}>{column}</th>)}</tr></thead><tbody>{rows.map((row,index)=><tr key={index}>{row.map((value,column)=><td key={column}>{value}</td>)}</tr>)}</tbody></table>:<p>{query.data===undefined?(query.isError?"Unavailable: this data could not be loaded.":"Loading…"):"No series returned."}</p>}</section> }
