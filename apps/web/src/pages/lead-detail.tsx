import { useState, useCallback, useEffect, useRef } from "react"
import { useParams, useNavigate } from "react-router-dom"
import { toast } from "sonner"
import {
  ArrowLeft, Mail, Phone, Globe, Link2, AtSign,
  MapPin, Building2, User, Calendar, Sparkles, Search,
  FileText, Loader2, CheckCircle, AlertCircle, Pencil,
  Trash2, ExternalLink, Zap, TrendingUp, ChevronDown,
  ChevronRight,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { EditableCell } from "@/components/editable-cell"
import { MarkdownContent } from "@/components/markdown-content"
import { useLead, useUpdateLead, useUpdateStatus, useDeleteLead } from "@/lib/hooks"
import { enrichLead, type EnrichAction } from "@/lib/api"
import { useQueryClient } from "@tanstack/react-query"
import { queryKeys } from "@/lib/query-client"

const TIER_COLOR: Record<string, string> = {
  hot: "#ef4444", warm: "#f97316", cold: "#3b82f6", unqualified: "#71717a",
}
const TIER_BG: Record<string, string> = {
  hot: "bg-red-500/10 text-red-500 border-red-500/20",
  warm: "bg-orange-500/10 text-orange-500 border-orange-500/20",
  cold: "bg-blue-500/10 text-blue-500 border-blue-500/20",
  unqualified: "bg-muted text-muted-foreground",
}
const STATUS_OPTIONS = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]

const KEY_FIELDS = ["email", "phone", "website", "contact_person", "linkedin_url", "description", "company_size"] as const

// ── Score Ring ──────────────────────────────────────────────────
function ScoreRing({ score, tier }: { score: number; tier: string }) {
  const r = 22, sw = 4
  const c = 2 * Math.PI * r
  const offset = c - (score / 100) * c
  const color = TIER_COLOR[tier] || TIER_COLOR.cold
  return (
    <div className="relative size-14 shrink-0" title={`${score}/100 (${tier})`}>
      <svg viewBox="0 0 52 52" className="size-full -rotate-90">
        <circle cx="26" cy="26" r={r} fill="none" stroke="currentColor" strokeWidth={sw} className="text-muted/30" />
        <circle cx="26" cy="26" r={r} fill="none" stroke={color} strokeWidth={sw}
          strokeDasharray={c} strokeDashoffset={offset} strokeLinecap="round"
          className="transition-all duration-700" />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="text-sm font-bold tabular-nums" style={{ color }}>{score}</span>
        <span className="text-[9px] text-muted-foreground">{tier}</span>
      </div>
    </div>
  )
}

// ── Completeness Bar ────────────────────────────────────────────
function CompletenessBar({ lead }: { lead: any }) {
  const filled = KEY_FIELDS.filter(f => lead[f]).length
  const pct = Math.round((filled / KEY_FIELDS.length) * 100)
  const missing = KEY_FIELDS.filter(f => !lead[f]).map(f => f.replace(/_/g, " "))
  return (
    <div className="flex items-center gap-2" title={missing.length ? `Missing: ${missing.join(", ")}` : "All key fields filled"}>
      <div className="flex-1 h-1.5 bg-muted rounded-full overflow-hidden max-w-[200px]">
        <div className="h-full bg-primary/60 rounded-full transition-all duration-500" style={{ width: `${pct}%` }} />
      </div>
      <span className="text-[10px] text-muted-foreground tabular-nums">{pct}%</span>
    </div>
  )
}

// ── Property Row ────────────────────────────────────────────────
function PropertyRow({ icon, label, children, provenance }: {
  icon: React.ReactNode; label: string; children: React.ReactNode; provenance?: string
}) {
  return (
    <div className="flex items-center gap-2 py-1.5 px-1 rounded hover:bg-muted/30 transition-colors group min-h-[32px]">
      <div className="text-muted-foreground/60 shrink-0">{icon}</div>
      <div className="text-xs text-muted-foreground w-20 shrink-0">{label}</div>
      <div className="flex items-center gap-1.5 min-w-0 flex-1">{children}</div>
      {provenance && (
        <span className="text-[9px] text-muted-foreground/40 shrink-0 opacity-0 group-hover:opacity-100 transition-opacity">
          via {provenance}
        </span>
      )}
    </div>
  )
}

// ── Decision Maker Card ─────────────────────────────────────────
function DecisionMakerCard({ dm }: { dm: { name?: string; title?: string; linkedin?: string; email?: string } }) {
  return (
    <div className="flex items-center gap-2.5 p-2.5 rounded-lg border bg-card hover:bg-muted/30 transition-colors">
      <div className="size-8 rounded-full bg-primary/10 flex items-center justify-center shrink-0">
        <User className="size-3.5 text-primary/60" />
      </div>
      <div className="min-w-0 flex-1">
        <div className="text-sm font-medium truncate">{dm.name || "Unknown"}</div>
        {dm.title && <div className="text-[11px] text-muted-foreground truncate">{dm.title}</div>}
      </div>
      {dm.linkedin && (
        <a href={dm.linkedin} target="_blank" rel="noopener" onClick={e => e.stopPropagation()}
          className="text-muted-foreground hover:text-foreground shrink-0">
          <ExternalLink className="size-3" />
        </a>
      )}
    </div>
  )
}

// ── Enrich Button ───────────────────────────────────────────────
function EnrichButton({ icon, label, desc, loading, disabled, onClick }: {
  icon: React.ReactNode; label: string; desc: string; loading: boolean; disabled: boolean; onClick: () => void
}) {
  return (
    <button className="w-full flex items-center gap-3 rounded-lg border border-dashed p-3 text-left transition-colors hover:bg-muted/50 disabled:opacity-40 disabled:pointer-events-none"
      onClick={onClick} disabled={disabled}>
      <div className="shrink-0 text-muted-foreground">
        {loading ? <Loader2 className="size-4 animate-spin" /> : icon}
      </div>
      <div className="min-w-0">
        <div className="text-sm font-medium">{label}</div>
        <div className="text-[11px] text-muted-foreground">{desc}</div>
      </div>
    </button>
  )
}

// ── Timeline Item ───────────────────────────────────────────────
function TimelineItem({ label, date, icon }: { label: string; date?: string | null; icon?: React.ReactNode }) {
  return (
    <div className="flex items-center gap-3">
      <div className="size-6 rounded-full bg-muted flex items-center justify-center shrink-0">
        {icon || <Calendar className="size-3 text-muted-foreground" />}
      </div>
      <div className="flex-1 min-w-0">
        <div className="text-xs font-medium">{label}</div>
        <div className="text-[11px] text-muted-foreground">
          {date ? new Date(date).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric", hour: "2-digit", minute: "2-digit" }) : "—"}
        </div>
      </div>
    </div>
  )
}

// ── Favicon Helper ──────────────────────────────────────────────
function CompanyAvatar({ company, website }: { company: string; website?: string }) {
  let domain: string | undefined
  if (website) {
    try { domain = new URL(website.startsWith("http") ? website : `https://${website}`).hostname.replace("www.", "") } catch {}
  }
  const initials = (company || "?").split(" ").slice(0, 2).map(w => w[0]).join("").toUpperCase()
  const [imgFailed, setImgFailed] = useState(false)
  return (
    <div className="size-10 rounded-lg bg-muted flex items-center justify-center shrink-0 overflow-hidden border">
      {domain && !imgFailed ? (
        <img src={`https://www.google.com/s2/favicons?domain=${domain}&sz=32`} alt="" className="size-7"
          onError={() => setImgFailed(true)} />
      ) : (
        <span className="text-xs font-bold text-muted-foreground">{initials}</span>
      )}
    </div>
  )
}
// ── Leaflet Map ─────────────────────────────────────────────────
import L from "leaflet"
import "leaflet/dist/leaflet.css"
import markerIcon2x from "leaflet/dist/images/marker-icon-2x.png"
import markerIcon from "leaflet/dist/images/marker-icon.png"
import markerShadow from "leaflet/dist/images/marker-shadow.png"
delete (L.Icon.Default.prototype as any)._getIconUrl
L.Icon.Default.mergeOptions({ iconUrl: markerIcon, iconRetinaUrl: markerIcon2x, shadowUrl: markerShadow })

function LeafletMap({ query, show, label, fallbackQueries = [] }: { query: string; show: boolean; label?: string; fallbackQueries?: string[] }) {
  const mapRef = useRef<HTMLDivElement>(null)
  const mapInstanceRef = useRef<L.Map | null>(null)
  const [loading, setLoading] = useState(true)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    if (!show || !query || !mapRef.current) return
    const controller = new AbortController()
    const queries = [query, ...fallbackQueries].filter(Boolean)

    async function geocodeAndRender() {
      for (const q of queries) {
        try {
          const res = await fetch(
            `https://nominatim.openstreetmap.org/search?format=json&q=${encodeURIComponent(q)}&limit=1`,
            { signal: controller.signal, headers: { Accept: "application/json" } }
          )
          const data = await res.json()
          if (data?.[0] && mapRef.current) {
            const lat = parseFloat(data[0].lat), lon = parseFloat(data[0].lon)
            if (mapInstanceRef.current) { mapInstanceRef.current.remove(); mapInstanceRef.current = null }
            const map = L.map(mapRef.current, { zoomControl: false, attributionControl: false }).setView([lat, lon], 14)
            L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png").addTo(map)
            L.control.zoom({ position: "bottomright" }).addTo(map)
            const marker = L.marker([lat, lon]).addTo(map)
            if (label) marker.bindPopup(`<b>${label}</b>`).openPopup()
            mapInstanceRef.current = map
            setLoading(false)
            return
          }
        } catch { if (controller.signal.aborted) return }
      }
      setFailed(true); setLoading(false)
    }
    geocodeAndRender()
    return () => { controller.abort(); if (mapInstanceRef.current) { mapInstanceRef.current.remove(); mapInstanceRef.current = null } }
  }, [query, show, label, fallbackQueries.join(",")])

  if (!show || !query) return null
  return (
    <div className="space-y-1.5">
      <div className="rounded-lg overflow-hidden border shadow-sm relative">
        {loading && <div className="absolute inset-0 z-10 flex items-center justify-center text-xs text-muted-foreground bg-muted/40">Loading map…</div>}
        {failed && <div className="h-[140px] flex items-center justify-center text-xs text-muted-foreground bg-muted/20">Could not locate on map</div>}
        <div ref={mapRef} style={{ height: 140, width: "100%" }} className={failed ? "hidden" : ""} />
      </div>
      <a href={`https://www.openstreetmap.org/search?query=${encodeURIComponent(query)}`} target="_blank" rel="noopener noreferrer"
        className="inline-flex items-center gap-1 text-[10px] text-muted-foreground hover:text-foreground transition-colors">
        <ExternalLink className="size-2.5" /> OpenStreetMap
      </a>
    </div>
  )
}

// ═══════════════════════════════════════════════════════════════
//  MAIN PAGE COMPONENT
// ═══════════════════════════════════════════════════════════════

export default function LeadDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const leadId = Number(id)
  const qc = useQueryClient()
  const { data: lead, isLoading } = useLead(leadId)
  const updateLead = useUpdateLead()
  const updateStatus = useUpdateStatus()
  const deleteLead = useDeleteLead()

  const [enriching, setEnriching] = useState<string | null>(null)
  const [researchContent, setResearchContent] = useState("")
  const [enrichLog, setEnrichLog] = useState<Array<{ step: string; message?: string }>>([])
  const [showEnrich, setShowEnrich] = useState(false)

  const handleEnrich = useCallback(async (action: EnrichAction) => {
    if (enriching) return
    setEnriching(action)
    setEnrichLog([])
    if (action === "web_research") setResearchContent("")
    try {
      await enrichLead(leadId, action, (event) => {
        const step = event.step as string
        if (step === "token") setResearchContent(prev => prev + (event.content as string))
        else if (step === "start" || step === "result" || step === "saved") setEnrichLog(prev => [...prev, { step, message: event.message as string }])
        else if (step === "error") { toast.error(event.message as string); setEnrichLog(prev => [...prev, { step: "error", message: event.message as string }]) }
        else if (step === "done") {
          qc.invalidateQueries({ queryKey: queryKeys.leads.detail(leadId) })
          qc.invalidateQueries({ queryKey: queryKeys.leads.all })
          toast.success(`${action.replace("_", " ")} completed`)
        }
      })
    } catch { toast.error("Enrichment failed") }
    finally { setEnriching(null) }
  }, [leadId, enriching, qc])

  const saveField = (field: string, value: string) => updateLead.mutate({ id: leadId, fields: { [field]: value } })

  if (isLoading) return (
    <div className="p-6 space-y-4">
      <div className="flex items-center gap-4"><Skeleton className="size-10 rounded-lg" /><Skeleton className="h-8 w-64" /></div>
      <Skeleton className="h-[400px]" />
    </div>
  )

  if (!lead) return (
    <div className="p-6 flex flex-col items-center justify-center gap-4 min-h-[60vh]">
      <AlertCircle className="size-12 text-muted-foreground" />
      <p className="text-lg text-muted-foreground">Lead not found</p>
      <Button variant="outline" onClick={() => navigate("/leads")}><ArrowLeft className="size-4 mr-2" /> Back to Leads</Button>
    </div>
  )

  const tier = lead.score_tier || "cold"
  const location = [lead.city, lead.state].filter(Boolean).join(", ")

  // Parse decision makers
  let decisionMakers: Array<{ name?: string; title?: string; linkedin?: string; email?: string }> = []
  try { if (lead.decision_makers) decisionMakers = JSON.parse(lead.decision_makers) } catch {}

  // Parse hiring signals
  let hiringSignals: any = null
  try { if ((lead as any).hiring_signals) hiringSignals = JSON.parse((lead as any).hiring_signals) } catch {}

  return (
    <div className="h-full overflow-y-auto">
      <div className="max-w-5xl mx-auto p-6 space-y-5">

        {/* ═══ HERO HEADER ═══ */}
        <div className="space-y-3">
          <div className="flex items-center justify-between">
            <Button variant="ghost" size="sm" onClick={() => navigate("/leads")} className="text-muted-foreground">
              <ArrowLeft className="size-4 mr-1" /> Leads
            </Button>
            <Button variant="ghost" size="icon" className="text-destructive/60 hover:text-destructive"
              onClick={() => { if (window.confirm(`Delete ${lead.company}?`)) { deleteLead.mutate(leadId, { onSuccess: () => { toast.success("Deleted"); navigate("/leads") } }) } }}>
              <Trash2 className="size-4" />
            </Button>
          </div>

          <div className="flex items-center gap-4">
            <CompanyAvatar company={lead.company} website={lead.website} />
            <div className="flex-1 min-w-0">
              <h1 className="text-xl font-bold tracking-tight truncate">{lead.company}</h1>
              <div className="flex items-center gap-2 mt-0.5 flex-wrap text-sm text-muted-foreground">
                {lead.specialization && <span>{lead.specialization}</span>}
                {location && <><span>·</span><span className="flex items-center gap-0.5"><MapPin className="size-3" />{location}</span></>}
                {lead.website && (
                  <><span>·</span>
                  <a href={lead.website.startsWith("http") ? lead.website : `https://${lead.website}`} target="_blank" rel="noopener"
                    className="flex items-center gap-0.5 hover:text-foreground transition-colors">
                    <Globe className="size-3" />{(() => { try { return new URL(lead.website.startsWith("http") ? lead.website : `https://${lead.website}`).hostname.replace("www.", "") } catch { return lead.website } })()}
                  </a></>
                )}
              </div>
            </div>
            <ScoreRing score={lead.score} tier={tier} />
          </div>

          {/* Quick actions row */}
          <div className="flex items-center gap-2 flex-wrap">
            <Badge variant="outline" className={TIER_BG[tier]}>{lead.score} · {tier}</Badge>
            <Select value={lead.status || "new"} onValueChange={(v) => { updateStatus.mutate({ id: leadId, status: v }); toast.success(`Status → ${v}`) }}>
              <SelectTrigger className="h-6 w-auto text-xs border-dashed gap-1 px-2"><SelectValue /></SelectTrigger>
              <SelectContent>{STATUS_OPTIONS.map(s => <SelectItem key={s} value={s}>{s}</SelectItem>)}</SelectContent>
            </Select>
            {lead.source && <span className="text-[11px] text-muted-foreground">via {lead.source}</span>}
            <div className="ml-auto">
              <Button variant={showEnrich ? "secondary" : "outline"} size="sm" onClick={() => setShowEnrich(!showEnrich)} className="h-7 text-xs gap-1.5">
                <Sparkles className="size-3" /> {showEnrich ? "Hide" : "Enrich"}
                {showEnrich ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
              </Button>
            </div>
          </div>

          <CompletenessBar lead={lead} />
        </div>

        {/* ═══ ENRICHMENT PANEL (collapsible) ═══ */}
        {showEnrich && (
          <div className="rounded-lg border border-dashed p-4 space-y-2 bg-card/50 animate-in fade-in slide-in-from-top-2 duration-200">
            <div className="text-xs font-medium text-muted-foreground flex items-center gap-1.5 mb-2">
              <Sparkles className="size-3 text-amber-500" /> Enrich with AI
            </div>
            <div className="grid grid-cols-2 md:grid-cols-5 gap-2">
              <EnrichButton icon={<Search className="size-4" />} label="Research" desc="AI analysis" loading={enriching === "web_research"} disabled={!!enriching} onClick={() => handleEnrich("web_research")} />
              <EnrichButton icon={<Mail className="size-4" />} label="Find Email" desc="Email patterns" loading={enriching === "find_emails"} disabled={!!enriching} onClick={() => handleEnrich("find_emails")} />
              <EnrichButton icon={<Globe className="size-4" />} label="Scrape Web" desc="Website data" loading={enriching === "scrape_website"} disabled={!!enriching || !lead.website} onClick={() => handleEnrich("scrape_website")} />
              <EnrichButton icon={<Phone className="size-4" />} label="Find Phone" desc="Contact number" loading={enriching === "find_phone"} disabled={!!enriching} onClick={() => handleEnrich("find_phone")} />
              <EnrichButton icon={<MapPin className="size-4" />} label="Find Address" desc="OSM + web" loading={enriching === "find_address"} disabled={!!enriching} onClick={() => handleEnrich("find_address")} />
            </div>
            {enrichLog.length > 0 && (
              <div className="space-y-1 pt-2 border-t mt-2">
                {enrichLog.map((log, i) => (
                  <div key={i} className="flex items-start gap-1.5 text-[11px]">
                    {log.step === "error" ? <AlertCircle className="size-3 text-destructive shrink-0 mt-0.5" /> : <CheckCircle className="size-3 text-emerald-500 shrink-0 mt-0.5" />}
                    <span className="text-muted-foreground">{log.message}</span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}

        {/* ═══ TABS ═══ */}
        <Tabs defaultValue="overview" className="space-y-4">
          <TabsList>
            <TabsTrigger value="overview">Overview</TabsTrigger>
            <TabsTrigger value="research">Research</TabsTrigger>
            <TabsTrigger value="activity">Activity</TabsTrigger>
          </TabsList>

          {/* ── OVERVIEW TAB ── */}
          <TabsContent value="overview">
            <div className="grid grid-cols-1 lg:grid-cols-5 gap-5">
              {/* Left: Properties */}
              <div className="lg:col-span-3 space-y-1">
                <div className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider px-1 pt-2 pb-1">Contact</div>
                <PropertyRow icon={<User className="size-3.5" />} label="Person">
                  <EditableCell value={lead.contact_person || ""} onSave={v => saveField("contact_person", v)} placeholder="Add contact name" />
                </PropertyRow>
                <PropertyRow icon={<FileText className="size-3.5" />} label="Title">
                  <EditableCell value={lead.contact_title || ""} onSave={v => saveField("contact_title", v)} placeholder="Add title" />
                </PropertyRow>
                <PropertyRow icon={<Mail className="size-3.5" />} label="Email" provenance={lead.email_provider || undefined}>
                  <EditableCell value={lead.email || ""} onSave={v => saveField("email", v)} placeholder="Add email" />
                  {lead.email && <a href={`mailto:${lead.email}`} className="text-muted-foreground hover:text-foreground"><ExternalLink className="size-3" /></a>}
                  {lead.email_confidence && (
                    <Badge variant="secondary" className={`text-[9px] px-1 py-0 ${
                      lead.email_confidence === "smtp_verified" ? "bg-emerald-600/20 text-emerald-500 border-emerald-500/40" :
                      lead.email_confidence === "verified" ? "bg-emerald-500/15 text-emerald-600 border-emerald-500/30" :
                      lead.email_confidence === "pattern" ? "bg-amber-500/15 text-amber-600 border-amber-500/30" : ""
                    }`}>{lead.email_confidence === "smtp_verified" ? "✓ SMTP" : lead.email_confidence}</Badge>
                  )}
                </PropertyRow>
                <PropertyRow icon={<Phone className="size-3.5" />} label="Phone" provenance={lead.phone_provider || undefined}>
                  <EditableCell value={lead.phone || ""} onSave={v => saveField("phone", v)} placeholder="Add phone" />
                </PropertyRow>
                <PropertyRow icon={<Link2 className="size-3.5" />} label="LinkedIn">
                  <EditableCell value={lead.linkedin_url || ""} onSave={v => saveField("linkedin_url", v)} placeholder="Add LinkedIn URL" />
                  {lead.linkedin_url && <a href={lead.linkedin_url} target="_blank" rel="noopener" className="text-muted-foreground hover:text-foreground"><ExternalLink className="size-3" /></a>}
                </PropertyRow>
                <PropertyRow icon={<AtSign className="size-3.5" />} label="Twitter">
                  <EditableCell value={lead.twitter_url || ""} onSave={v => saveField("twitter_url", v)} placeholder="Add Twitter" />
                </PropertyRow>

                <Separator className="my-2" />
                <div className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider px-1 pt-1 pb-1">Company</div>
                <PropertyRow icon={<Globe className="size-3.5" />} label="Website">
                  <EditableCell value={lead.website || ""} onSave={v => saveField("website", v)} placeholder="Add website" />
                  {lead.website && <a href={lead.website.startsWith("http") ? lead.website : `https://${lead.website}`} target="_blank" rel="noopener" className="text-muted-foreground hover:text-foreground"><ExternalLink className="size-3" /></a>}
                </PropertyRow>
                <PropertyRow icon={<MapPin className="size-3.5" />} label="Location">
                  <span className="text-sm">{location || "—"}</span>
                </PropertyRow>
                <PropertyRow icon={<Building2 className="size-3.5" />} label="Size">
                  <EditableCell value={lead.company_size || ""} onSave={v => saveField("company_size", v)} placeholder="e.g. 50-200" />
                </PropertyRow>
                <PropertyRow icon={<Zap className="size-3.5" />} label="Industry">
                  <EditableCell value={lead.specialization || ""} onSave={v => saveField("specialization", v)} placeholder="Add specialization" />
                </PropertyRow>
                <PropertyRow icon={<Calendar className="size-3.5" />} label="Founded">
                  <EditableCell value={lead.founded_year || ""} onSave={v => saveField("founded_year", v)} placeholder="Add year" />
                </PropertyRow>
                <PropertyRow icon={<TrendingUp className="size-3.5" />} label="Revenue">
                  <EditableCell value={lead.revenue_range || ""} onSave={v => saveField("revenue_range", v)} placeholder="Add revenue" />
                </PropertyRow>
                {lead.address && (
                  <PropertyRow icon={<MapPin className="size-3.5" />} label="Address">
                    <span className="text-sm">{lead.address}</span>
                  </PropertyRow>
                )}

                <Separator className="my-2" />
                <div className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider px-1 pt-1 pb-1">Social</div>
                <PropertyRow icon={<Link2 className="size-3.5" />} label="LinkedIn">
                  {lead.linkedin_url ? <a href={lead.linkedin_url} target="_blank" rel="noopener" className="text-sm text-blue-400 hover:underline truncate">{lead.linkedin_url}</a> : <span className="text-sm text-muted-foreground/40">—</span>}
                </PropertyRow>
                <PropertyRow icon={<Globe className="size-3.5" />} label="Facebook">
                  <EditableCell value={lead.facebook_url || ""} onSave={v => saveField("facebook_url", v)} placeholder="Add Facebook" />
                </PropertyRow>
              </div>

              {/* Right: Map + Decision Makers + Hiring + Notes */}
              <div className="lg:col-span-2 space-y-4">
                <LeafletMap
                  query={[lead.address, lead.city, lead.state].filter(Boolean).join(", ")}
                  fallbackQueries={[[lead.city, lead.state].filter(Boolean).join(", "), lead.city || ""]}
                  show={!!(lead.city || lead.address)} label={lead.company}
                />

                {decisionMakers.length > 0 && (
                  <div className="space-y-2">
                    <div className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider">Decision Makers</div>
                    {decisionMakers.map((dm, i) => <DecisionMakerCard key={i} dm={dm} />)}
                  </div>
                )}

                {hiringSignals && hiringSignals.total_jobs > 0 && (
                  <div className="rounded-lg border p-3 space-y-2">
                    <div className="flex items-center justify-between">
                      <span className="text-xs font-medium flex items-center gap-1.5"><TrendingUp className="size-3.5 text-green-500" /> Hiring Signals</span>
                      <Badge variant="outline" className="text-[9px] px-1 py-0">{hiringSignals.growth_signal?.replace("_", " ")}</Badge>
                    </div>
                    <div className="text-sm"><span className="text-muted-foreground">Open positions:</span> <span className="font-medium">{hiringSignals.total_jobs}</span></div>
                    <div className="flex flex-wrap gap-1">
                      {hiringSignals.gtm_expansion && <Badge variant="outline" className="text-[9px] bg-purple-500/10 text-purple-400">🎯 GTM</Badge>}
                      {hiringSignals.tech_hiring && <Badge variant="outline" className="text-[9px] bg-cyan-500/10 text-cyan-400">💻 Tech</Badge>}
                    </div>
                    {hiringSignals.roles?.length > 0 && (
                      <div className="flex flex-wrap gap-1">{hiringSignals.roles.map((r: string, i: number) => <Badge key={i} variant="secondary" className="text-[9px] px-1 py-0">{r}</Badge>)}</div>
                    )}
                  </div>
                )}

                {/* Notes */}
                <div className="space-y-1.5">
                  <div className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider flex items-center gap-1">
                    <Pencil className="size-3" /> Notes
                  </div>
                  <EditableCell value={lead.notes || ""} onSave={v => saveField("notes", v)} placeholder="Add notes about this lead..." className="text-sm min-h-[60px]" />
                </div>
              </div>
            </div>
          </TabsContent>

          {/* ── RESEARCH TAB ── */}
          <TabsContent value="research" className="space-y-4">
            <div className="rounded-lg border p-5">
              <div className="flex items-center justify-between mb-3">
                <h3 className="text-sm font-medium flex items-center gap-2"><Sparkles className="size-4 text-amber-500" /> AI Research</h3>
                {lead.last_enriched_at && <span className="text-[11px] text-muted-foreground">Last: {new Date(lead.last_enriched_at).toLocaleDateString()}</span>}
              </div>
              {(researchContent || lead.description) ? (
                <div className="text-sm leading-relaxed">
                  <MarkdownContent content={researchContent || lead.description} />
                  {enriching === "web_research" && <span className="inline-block w-2 h-4 bg-foreground/60 animate-pulse ml-0.5" />}
                </div>
              ) : (
                <p className="text-sm text-muted-foreground italic">No research yet. Use the Enrich button to generate an AI analysis.</p>
              )}
            </div>
            {lead.yupcha_value_prop && (
              <div className="rounded-lg border p-5">
                <h3 className="text-sm font-medium mb-2">Value Proposition</h3>
                <MarkdownContent content={lead.yupcha_value_prop} className="text-sm text-muted-foreground" />
              </div>
            )}
            {lead.company_need && (
              <div className="rounded-lg border p-5">
                <h3 className="text-sm font-medium mb-2">Company Need</h3>
                <MarkdownContent content={lead.company_need} className="text-sm text-muted-foreground" />
              </div>
            )}
          </TabsContent>

          {/* ── ACTIVITY TAB ── */}
          <TabsContent value="activity">
            <div className="rounded-lg border p-5 space-y-4">
              <h3 className="text-sm font-medium">Timeline</h3>
              <div className="space-y-3">
                <TimelineItem label="Created" date={lead.created_at} />
                <TimelineItem label="Last Updated" date={lead.updated_at} />
                <TimelineItem label="Last Enriched" date={lead.last_enriched_at} icon={<Sparkles className="size-3 text-amber-500" />} />
                {lead.source && (
                  <div className="flex items-center gap-3">
                    <div className="size-6 rounded-full bg-muted flex items-center justify-center shrink-0"><Zap className="size-3 text-muted-foreground" /></div>
                    <div className="flex-1"><div className="text-xs font-medium">Source</div><div className="text-[11px] text-muted-foreground">{lead.source}</div></div>
                  </div>
                )}
                {lead.workspace_id && (
                  <div className="flex items-center gap-3">
                    <div className="size-6 rounded-full bg-muted flex items-center justify-center shrink-0"><Building2 className="size-3 text-muted-foreground" /></div>
                    <div className="flex-1"><div className="text-xs font-medium">Workspace</div><div className="text-[11px] text-muted-foreground font-mono">{lead.workspace_id}</div></div>
                  </div>
                )}
              </div>
            </div>
          </TabsContent>
        </Tabs>
      </div>
    </div>
  )
}
