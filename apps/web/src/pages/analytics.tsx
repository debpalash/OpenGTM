import { useMemo } from "react"
import {
  BarChart3, TrendingUp, Users, Mail, Phone, Globe,
  Link2, Target, Zap, Brain, CheckCircle2,
  ArrowUp, ArrowDown, Activity, Database, Sparkles,
} from "lucide-react"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Skeleton } from "@/components/ui/skeleton"
import { Separator } from "@/components/ui/separator"
import {
  useAnalyticsOverview, useAnalyticsPipeline,
  useAnalyticsCollection, useAnalyticsEnrichment, useAnalyticsLLM,
} from "@/lib/hooks"
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip,
  ResponsiveContainer, PieChart, Pie, Cell, AreaChart, Area,
} from "recharts"


// ── Color Palette ────────────────────────────────────────────────

const TIER_COLORS: Record<string, string> = {
  hot: "#ef4444",
  warm: "#f59e0b",
  cold: "#3b82f6",
  unqualified: "#6b7280",
}

const STATUS_COLORS: Record<string, string> = {
  new: "#8b5cf6",
  contacted: "#3b82f6",
  qualified: "#10b981",
  negotiating: "#f59e0b",
  converted: "#22c55e",
  dead: "#6b7280",
}

const CHART_COLORS = [
  "#8b5cf6", "#3b82f6", "#10b981", "#f59e0b",
  "#ef4444", "#ec4899", "#06b6d4", "#84cc16",
]

const tooltipStyle = {
  contentStyle: {
    backgroundColor: "hsl(var(--card))",
    border: "1px solid hsl(var(--border))",
    borderRadius: "8px",
    fontSize: "12px",
    color: "hsl(var(--foreground))",
  },
}


// ── KPI Card ─────────────────────────────────────────────────────

function KPICard({
  title, value, subtitle, icon: Icon, trend, color = "text-primary",
}: {
  title: string
  value: string | number
  subtitle?: string
  icon: typeof BarChart3
  trend?: { value: number; label: string }
  color?: string
}) {
  return (
    <Card>
      <CardContent className="pt-5 pb-4">
        <div className="flex items-start justify-between">
          <div className="space-y-1">
            <p className="text-xs font-medium text-muted-foreground uppercase tracking-wider">{title}</p>
            <p className="text-2xl font-bold tracking-tight">{value}</p>
            {subtitle && <p className="text-xs text-muted-foreground">{subtitle}</p>}
          </div>
          <div className={`p-2.5 rounded-xl bg-primary/10 ${color}`}>
            <Icon className="size-5" />
          </div>
        </div>
        {trend && (
          <div className="flex items-center gap-1 mt-2 text-xs">
            {trend.value >= 0 ? (
              <ArrowUp className="size-3 text-emerald-500" />
            ) : (
              <ArrowDown className="size-3 text-red-500" />
            )}
            <span className={trend.value >= 0 ? "text-emerald-500" : "text-red-500"}>
              {trend.value >= 0 ? "+" : ""}{trend.value}
            </span>
            <span className="text-muted-foreground">{trend.label}</span>
          </div>
        )}
      </CardContent>
    </Card>
  )
}


// ── Enrichment Coverage Bar ──────────────────────────────────────

function CoverageBar({ label, icon: Icon, value, pct }: {
  label: string; icon: typeof Mail; value: number; pct: number
}) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-xs">
        <div className="flex items-center gap-1.5 text-muted-foreground">
          <Icon className="size-3.5" />
          <span>{label}</span>
        </div>
        <span className="font-medium">{value} <span className="text-muted-foreground">({pct}%)</span></span>
      </div>
      <div className="h-2 rounded-full bg-muted overflow-hidden">
        <div
          className="h-full rounded-full bg-gradient-to-r from-primary/80 to-primary transition-all duration-700 ease-out"
          style={{ width: `${Math.min(pct, 100)}%` }}
        />
      </div>
    </div>
  )
}


// ── Main Component ───────────────────────────────────────────────

export default function AnalyticsPage() {
  const { data: overview, isLoading: lo } = useAnalyticsOverview()
  const { data: pipeline, isLoading: lp } = useAnalyticsPipeline()
  const { data: collection, isLoading: lc } = useAnalyticsCollection()
  const { data: enrichment, isLoading: le } = useAnalyticsEnrichment()
  const { data: llm, isLoading: ll } = useAnalyticsLLM()

  const isLoading = lo || lp || lc || le || ll

  // Pipeline funnel data
  const funnelData = useMemo(() => {
    if (!pipeline?.statuses) return []
    const order = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]
    return order
      .filter(s => (pipeline.statuses[s] || 0) > 0)
      .map(s => ({
        status: s.charAt(0).toUpperCase() + s.slice(1),
        count: pipeline.statuses[s] || 0,
        fill: STATUS_COLORS[s] || "#6b7280",
      }))
  }, [pipeline])

  // Tier donut data
  const tierData = useMemo(() => {
    if (!overview?.tiers) return []
    return Object.entries(overview.tiers)
      .filter(([, v]) => v > 0)
      .map(([k, v]) => ({ name: k, value: v, fill: TIER_COLORS[k] || "#6b7280" }))
  }, [overview])

  // Score distribution
  const scoreDist = useMemo(() => {
    if (!enrichment?.score_distribution) return []
    return enrichment.score_distribution.map(d => ({
      range: d.range,
      count: d.count,
    }))
  }, [enrichment])

  // Source quality
  const sourceData = useMemo(() => {
    if (!enrichment?.source_quality) return []
    return enrichment.source_quality.slice(0, 8).map(s => ({
      source: s.source.replace("job:", "").slice(0, 12),
      count: s.count,
      avg_score: s.avg_score,
    }))
  }, [enrichment])

  if (isLoading) {
    return (
      <div className="p-6 space-y-6">
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
          {[...Array(4)].map((_, i) => (
            <Skeleton key={i} className="h-28 rounded-xl" />
          ))}
        </div>
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
          <Skeleton className="h-72 rounded-xl" />
          <Skeleton className="h-72 rounded-xl" />
        </div>
      </div>
    )
  }

  return (
    <div className="p-6 space-y-6">
      {/* ── Header ──────────────────────────────────────────── */}
      <div>
        <h2 className="text-lg font-semibold flex items-center gap-2">
          <BarChart3 className="size-5 text-primary" />
          Analytics
        </h2>
        <p className="text-sm text-muted-foreground">
          Platform performance, enrichment quality, and pipeline metrics.
        </p>
      </div>

      {/* ── Row 1: KPI Cards ──────────────────────────────────── */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
        <KPICard
          title="Total Leads"
          value={overview?.total_leads || 0}
          icon={Database}
          trend={overview ? { value: overview.leads_this_week, label: "this week" } : undefined}
        />
        <KPICard
          title="Avg Score"
          value={overview?.avg_score || 0}
          subtitle={`${overview?.tiers?.hot || 0} hot, ${overview?.tiers?.warm || 0} warm`}
          icon={Target}
          color="text-amber-500"
        />
        <KPICard
          title="Enrichment Rate"
          value={`${overview?.enrichment?.email_pct || 0}%`}
          subtitle={`${overview?.enrichment?.with_email || 0} leads with email`}
          icon={Mail}
          color="text-emerald-500"
        />
        <KPICard
          title="Job Success"
          value={`${overview?.jobs?.success_rate || 0}%`}
          subtitle={`${overview?.jobs?.completed || 0}/${overview?.jobs?.total || 0} completed`}
          icon={Zap}
          color="text-violet-500"
        />
      </div>

      {/* ── Row 2: Pipeline Funnel + Tier Distribution ──────── */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
        <Card className="lg:col-span-2">
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Activity className="size-4" /> Pipeline Funnel
            </CardTitle>
          </CardHeader>
          <CardContent>
            {funnelData.length > 0 ? (
              <ResponsiveContainer width="100%" height={240}>
                <BarChart data={funnelData} layout="vertical" margin={{ left: 20 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                  <XAxis type="number" tick={{ fontSize: 11, fill: "hsl(var(--muted-foreground))" }} />
                  <YAxis dataKey="status" type="category" width={90} tick={{ fontSize: 11, fill: "hsl(var(--muted-foreground))" }} />
                  <Tooltip {...tooltipStyle} />
                  <Bar dataKey="count" radius={[0, 6, 6, 0]}>
                    {funnelData.map((e, i) => (
                      <Cell key={i} fill={e.fill} />
                    ))}
                  </Bar>
                </BarChart>
              </ResponsiveContainer>
            ) : (
              <div className="h-60 flex items-center justify-center text-sm text-muted-foreground">
                No pipeline data yet
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Sparkles className="size-4" /> Score Tiers
            </CardTitle>
          </CardHeader>
          <CardContent>
            {tierData.length > 0 ? (
              <>
                <ResponsiveContainer width="100%" height={160}>
                  <PieChart>
                    <Pie data={tierData} cx="50%" cy="50%" innerRadius={40} outerRadius={65} paddingAngle={3} dataKey="value">
                      {tierData.map((e, i) => (
                        <Cell key={i} fill={e.fill} />
                      ))}
                    </Pie>
                    <Tooltip {...tooltipStyle} />
                  </PieChart>
                </ResponsiveContainer>
                <div className="flex flex-wrap justify-center gap-3 mt-2">
                  {tierData.map(t => (
                    <div key={t.name} className="flex items-center gap-1.5 text-xs">
                      <div className="size-2.5 rounded-full" style={{ backgroundColor: t.fill }} />
                      <span className="text-muted-foreground capitalize">{t.name}</span>
                      <span className="font-medium">{t.value}</span>
                    </div>
                  ))}
                </div>
              </>
            ) : (
              <div className="h-60 flex items-center justify-center text-sm text-muted-foreground">
                No score data yet
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 3: Enrichment Coverage ──────────────────────── */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-medium flex items-center gap-2">
            <CheckCircle2 className="size-4" /> Enrichment Coverage
          </CardTitle>
        </CardHeader>
        <CardContent>
          {overview?.enrichment ? (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-x-8 gap-y-4">
              <CoverageBar label="Email" icon={Mail} value={overview.enrichment.with_email} pct={overview.enrichment.email_pct} />
              <CoverageBar label="Phone" icon={Phone} value={overview.enrichment.with_phone} pct={overview.enrichment.phone_pct} />
              <CoverageBar label="Website" icon={Globe} value={overview.enrichment.with_website} pct={overview.enrichment.website_pct} />
              <CoverageBar label="Contact Person" icon={Users} value={overview.enrichment.with_contact} pct={overview.enrichment.contact_pct} />
              <CoverageBar label="LinkedIn" icon={Link2} value={overview.enrichment.with_linkedin} pct={Math.round((overview.enrichment.with_linkedin / (overview.enrichment.total || 1)) * 100)} />
              {overview.email_confidence && Object.keys(overview.email_confidence).length > 0 && (
                <div className="space-y-1.5">
                  <p className="text-xs text-muted-foreground flex items-center gap-1.5">
                    <Brain className="size-3.5" /> Email Confidence
                  </p>
                  <div className="flex flex-wrap gap-1.5">
                    {Object.entries(overview.email_confidence).map(([k, v]) => (
                      <Badge key={k} variant={k === "verified" ? "default" : "secondary"} className="text-[10px]">
                        {k}: {v}
                      </Badge>
                    ))}
                  </div>
                </div>
              )}
            </div>
          ) : (
            <div className="h-20 flex items-center justify-center text-sm text-muted-foreground">
              No enrichment data
            </div>
          )}
        </CardContent>
      </Card>

      {/* ── Row 4: Trends ──────────────────────────────────── */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        {/* Leads collected per day */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <TrendingUp className="size-4" /> Leads Collected (30 days)
            </CardTitle>
          </CardHeader>
          <CardContent>
            {collection?.leads_by_day && collection.leads_by_day.length > 0 ? (
              <ResponsiveContainer width="100%" height={200}>
                <AreaChart data={collection.leads_by_day}>
                  <defs>
                    <linearGradient id="colorLeads" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="#8b5cf6" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="#8b5cf6" stopOpacity={0} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                  <XAxis
                    dataKey="day"
                    tick={{ fontSize: 10, fill: "hsl(var(--muted-foreground))" }}
                    tickFormatter={(v: string) => v.slice(5)}
                  />
                  <YAxis tick={{ fontSize: 10, fill: "hsl(var(--muted-foreground))" }} />
                  <Tooltip {...tooltipStyle} />
                  <Area type="monotone" dataKey="count" stroke="#8b5cf6" fillOpacity={1} fill="url(#colorLeads)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            ) : (
              <div className="h-48 flex items-center justify-center text-sm text-muted-foreground">
                No collection data yet
              </div>
            )}
          </CardContent>
        </Card>

        {/* Score distribution */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Target className="size-4" /> Score Distribution
            </CardTitle>
          </CardHeader>
          <CardContent>
            {scoreDist.length > 0 ? (
              <ResponsiveContainer width="100%" height={200}>
                <BarChart data={scoreDist}>
                  <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                  <XAxis dataKey="range" tick={{ fontSize: 10, fill: "hsl(var(--muted-foreground))" }} />
                  <YAxis tick={{ fontSize: 10, fill: "hsl(var(--muted-foreground))" }} />
                  <Tooltip {...tooltipStyle} />
                  <Bar dataKey="count" fill="#10b981" radius={[4, 4, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            ) : (
              <div className="h-48 flex items-center justify-center text-sm text-muted-foreground">
                No score data yet
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 5: Source Quality + LLM Usage ──────────────── */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        {/* Top cities */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Globe className="size-4" /> Top Cities
            </CardTitle>
          </CardHeader>
          <CardContent>
            {enrichment?.by_city && enrichment.by_city.length > 0 ? (
              <div className="space-y-2">
                {enrichment.by_city.slice(0, 8).map(c => (
                  <div key={c.city} className="flex items-center gap-3">
                    <span className="text-xs text-muted-foreground w-24 truncate">{c.city}</span>
                    <div className="flex-1 h-2 rounded-full bg-muted overflow-hidden">
                      <div
                        className="h-full rounded-full bg-blue-500/70 transition-all duration-500"
                        style={{ width: `${(c.count / (enrichment.by_city[0]?.count || 1)) * 100}%` }}
                      />
                    </div>
                    <span className="text-xs font-medium w-8 text-right">{c.count}</span>
                    <Badge variant="secondary" className="text-[10px] shrink-0">
                      avg {c.avg_score}
                    </Badge>
                  </div>
                ))}
              </div>
            ) : (
              <div className="h-48 flex items-center justify-center text-sm text-muted-foreground">
                No city data
              </div>
            )}
          </CardContent>
        </Card>

        {/* LLM usage */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Brain className="size-4" /> LLM Token Usage
            </CardTitle>
          </CardHeader>
          <CardContent>
            {llm?.by_day && llm.by_day.length > 0 ? (
              <>
                <div className="flex items-center gap-4 mb-3">
                  <Badge variant="outline" className="text-xs">
                    {((llm.total_tokens || 0) / 1000).toFixed(1)}K total tokens
                  </Badge>
                  <Badge variant="outline" className="text-xs">
                    {llm.total_calls || 0} API calls
                  </Badge>
                </div>
                <ResponsiveContainer width="100%" height={160}>
                  <AreaChart data={llm.by_day}>
                    <defs>
                      <linearGradient id="colorTokens" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="5%" stopColor="#06b6d4" stopOpacity={0.3} />
                        <stop offset="95%" stopColor="#06b6d4" stopOpacity={0} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                    <XAxis dataKey="day" tick={{ fontSize: 10, fill: "hsl(var(--muted-foreground))" }} tickFormatter={(v: string) => v.slice(5)} />
                    <YAxis tick={{ fontSize: 10, fill: "hsl(var(--muted-foreground))" }} tickFormatter={(v: number) => `${(v / 1000).toFixed(0)}K`} />
                    <Tooltip {...tooltipStyle} />
                    <Area type="monotone" dataKey="tokens" stroke="#06b6d4" fillOpacity={1} fill="url(#colorTokens)" strokeWidth={2} />
                  </AreaChart>
                </ResponsiveContainer>
              </>
            ) : (
              <div className="h-48 flex items-center justify-center text-sm text-muted-foreground">
                No LLM usage data
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 6: LLM Providers ──────────────────────────── */}
      {llm?.by_provider && llm.by_provider.length > 0 && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Zap className="size-4" /> Provider Breakdown
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-6 gap-3">
              {llm.by_provider.map(p => (
                <div key={p.provider} className="rounded-lg border p-3 space-y-1">
                  <p className="text-xs font-medium truncate">{p.provider}</p>
                  <p className="text-lg font-bold">{((p.tokens || 0) / 1000).toFixed(1)}K</p>
                  <p className="text-[10px] text-muted-foreground">{p.calls} calls</p>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
