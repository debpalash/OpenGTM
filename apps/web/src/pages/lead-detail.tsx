import { useState, useCallback, useEffect, useRef } from "react"
import { useParams, useNavigate } from "react-router-dom"
import { toast } from "sonner"
import {
  ArrowLeft, Mail, Phone, Globe, Link2, AtSign,
  MapPin, Building2, User, Calendar, Sparkles, Search,
  FileText, Loader2, CheckCircle, AlertCircle, Pencil,
  Trash2, ExternalLink, Zap,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"
import { EditableCell } from "@/components/editable-cell"
import { MarkdownContent } from "@/components/markdown-content"
import { useLead, useUpdateLead, useUpdateStatus, useDeleteLead } from "@/lib/hooks"
import { enrichLead, type EnrichAction } from "@/lib/api"
import { useQueryClient } from "@tanstack/react-query"
import { queryKeys } from "@/lib/query-client"

const TIER_COLORS: Record<string, string> = {
  hot: "bg-red-500/10 text-red-500 border-red-500/20",
  warm: "bg-orange-500/10 text-orange-500 border-orange-500/20",
  cold: "bg-blue-500/10 text-blue-500 border-blue-500/20",
  unqualified: "bg-muted text-muted-foreground",
}

const TIER_GLOW: Record<string, string> = {
  hot: "shadow-[0_0_30px_rgba(239,68,68,0.15)]",
  warm: "shadow-[0_0_30px_rgba(249,115,22,0.1)]",
  cold: "shadow-[0_0_20px_rgba(59,130,246,0.08)]",
  unqualified: "",
}

const STATUS_OPTIONS = ["new", "contacted", "qualified", "dead"]

// ── Leaflet Map with geocoded pin ────────────────────────────────
import L from "leaflet"
import "leaflet/dist/leaflet.css"

// Fix default marker icon paths (Vite doesn't bundle them correctly)
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

    // Try queries in order: full → fallbacks
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
            const lat = parseFloat(data[0].lat)
            const lon = parseFloat(data[0].lon)

            if (mapInstanceRef.current) { mapInstanceRef.current.remove(); mapInstanceRef.current = null }

            const map = L.map(mapRef.current, { zoomControl: false, attributionControl: false }).setView([lat, lon], 14)
            L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
              attribution: '© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a>',
            }).addTo(map)
            L.control.zoom({ position: "bottomright" }).addTo(map)

            const marker = L.marker([lat, lon]).addTo(map)
            if (label) marker.bindPopup(`<b>${label}</b>`).openPopup()

            mapInstanceRef.current = map
            setLoading(false)
            return
          }
        } catch {
          if (controller.signal.aborted) return
        }
      }
      setFailed(true)
      setLoading(false)
    }

    geocodeAndRender()

    return () => {
      controller.abort()
      if (mapInstanceRef.current) { mapInstanceRef.current.remove(); mapInstanceRef.current = null }
    }
  }, [query, show, label, fallbackQueries.join(",")])

  if (!show || !query) return null

  return (
    <div className="mt-3 space-y-2">
      <div className="rounded-xl overflow-hidden border border-border/40 shadow-sm relative">
        {loading && (
          <div className="absolute inset-0 z-10 flex items-center justify-center text-xs text-muted-foreground bg-muted/40">
            Loading map…
          </div>
        )}
        {failed && (
          <div className="h-[200px] flex items-center justify-center text-xs text-muted-foreground bg-muted/20">
            Could not locate address on map
          </div>
        )}
        <div ref={mapRef} style={{ height: 200, width: "100%" }} className={failed ? "hidden" : ""} />
      </div>
      <a
        href={`https://www.openstreetmap.org/search?query=${encodeURIComponent(query)}`}
        target="_blank"
        rel="noopener noreferrer"
        className="inline-flex items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground transition-colors"
      >
        <ExternalLink className="size-3" /> Open in OpenStreetMap
      </a>
    </div>
  )
}

export default function LeadDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const leadId = Number(id)
  const qc = useQueryClient()

  const { data: lead, isLoading } = useLead(leadId)
  const updateLead = useUpdateLead()
  const updateStatus = useUpdateStatus()
  const deleteLead = useDeleteLead()

  // AI Enrichment state
  const [enriching, setEnriching] = useState<string | null>(null)
  const [researchContent, setResearchContent] = useState("")
  const [enrichLog, setEnrichLog] = useState<Array<{ step: string; message?: string }>>([])

  const handleEnrich = useCallback(async (action: EnrichAction) => {
    if (enriching) return
    setEnriching(action)
    setEnrichLog([])
    if (action === "web_research") setResearchContent("")

    try {
      await enrichLead(leadId, action, (event) => {
        const step = event.step as string

        if (step === "token") {
          setResearchContent(prev => prev + (event.content as string))
        } else if (step === "start" || step === "result" || step === "saved") {
          setEnrichLog(prev => [...prev, { step, message: event.message as string }])
        } else if (step === "error") {
          toast.error(event.message as string)
          setEnrichLog(prev => [...prev, { step: "error", message: event.message as string }])
        } else if (step === "done") {
          qc.invalidateQueries({ queryKey: queryKeys.leads.detail(leadId) })
          qc.invalidateQueries({ queryKey: queryKeys.leads.all })
          toast.success(`${action.replace("_", " ")} completed`)
        }
      })
    } catch (err) {
      toast.error("Enrichment failed")
    } finally {
      setEnriching(null)
    }
  }, [leadId, enriching, qc])

  const handleDelete = () => {
    deleteLead.mutate(leadId, {
      onSuccess: () => {
        toast.success("Lead deleted")
        navigate("/leads")
      },
    })
  }

  const saveField = (field: string, value: string) => {
    updateLead.mutate({ id: leadId, fields: { [field]: value } })
  }

  if (isLoading) {
    return (
      <div className="p-6 space-y-6">
        <div className="flex items-center gap-4">
          <Skeleton className="h-10 w-10 rounded-full" />
          <Skeleton className="h-8 w-64" />
        </div>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {[1, 2, 3, 4].map(i => <Skeleton key={i} className="h-48" />)}
        </div>
      </div>
    )
  }

  if (!lead) {
    return (
      <div className="p-6 flex flex-col items-center justify-center gap-4 min-h-[60vh]">
        <AlertCircle className="size-12 text-muted-foreground" />
        <p className="text-lg text-muted-foreground">Lead not found</p>
        <Button variant="outline" onClick={() => navigate("/leads")}>
          <ArrowLeft className="size-4 mr-2" /> Back to Leads
        </Button>
      </div>
    )
  }

  const tier = lead.score_tier || "cold"

  return (
    <div className="p-6 space-y-6">
      {/* ── Header ──────────────────────────────────────────── */}
      <div className="flex items-start justify-between gap-4">
        <div className="flex items-center gap-4 min-w-0">
          <Button variant="ghost" size="icon" onClick={() => navigate("/leads")} className="shrink-0">
            <ArrowLeft className="size-5" />
          </Button>
          <div className="min-w-0">
            <h1 className="text-2xl font-bold tracking-tight truncate">{lead.company}</h1>
            <div className="flex items-center gap-2 mt-1 flex-wrap">
              <Badge variant="outline" className={TIER_COLORS[tier]}>
                {lead.score} · {tier}
              </Badge>
              <Select
                value={lead.status || "new"}
                onValueChange={(v) => {
                  updateStatus.mutate({ id: leadId, status: v })
                  toast.success(`Status → ${v}`)
                }}
              >
                <SelectTrigger className="h-6 w-auto text-xs border-dashed gap-1 px-2">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {STATUS_OPTIONS.map(s => (
                    <SelectItem key={s} value={s}>{s}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {lead.source && (
                <span className="text-xs text-muted-foreground">via {lead.source}</span>
              )}
            </div>
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <Button
            variant="ghost"
            size="icon"
            className="text-destructive hover:text-destructive"
            onClick={() => {
              if (window.confirm(`Delete ${lead.company}? This cannot be undone.`)) {
                handleDelete()
              }
            }}
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      </div>

      {/* ── Main Grid ───────────────────────────────────────── */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">

        {/* Left Column — Contact + Company */}
        <div className="lg:col-span-2 space-y-4">
          {/* Contact Card */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <User className="size-4 text-muted-foreground" /> Contact Info
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <InfoRow icon={<User className="size-4" />} label="Contact Person">
                  <EditableCell value={lead.contact_person || ""} onSave={(v) => saveField("contact_person", v)} placeholder="Add contact name" />
                </InfoRow>
                <InfoRow icon={<FileText className="size-4" />} label="Title">
                  <EditableCell value={lead.contact_title || ""} onSave={(v) => saveField("contact_title", v)} placeholder="Add title" />
                </InfoRow>
                <InfoRow icon={<Mail className="size-4" />} label="Email">
                  <EditableCell value={lead.email || ""} onSave={(v) => saveField("email", v)} placeholder="Add email" />
                  {lead.email && (
                    <a href={`mailto:${lead.email}`} className="ml-1 text-muted-foreground hover:text-foreground">
                      <ExternalLink className="size-3" />
                    </a>
                  )}
                  {lead.email_confidence && (
                    <Badge
                      variant={lead.email_confidence === "verified" ? "default" : "secondary"}
                      className={`ml-1.5 text-[10px] px-1.5 py-0 ${
                        lead.email_confidence === "verified" ? "bg-emerald-500/15 text-emerald-600 border-emerald-500/30" :
                        lead.email_confidence === "pattern" ? "bg-amber-500/15 text-amber-600 border-amber-500/30" :
                        "bg-muted text-muted-foreground"
                      }`}
                    >
                      {lead.email_confidence}
                    </Badge>
                  )}
                </InfoRow>
                <InfoRow icon={<Phone className="size-4" />} label="Phone">
                  <EditableCell value={lead.phone || ""} onSave={(v) => saveField("phone", v)} placeholder="Add phone" />
                </InfoRow>
                <InfoRow icon={<Link2 className="size-4" />} label="LinkedIn">
                  <EditableCell value={lead.linkedin_url || ""} onSave={(v) => saveField("linkedin_url", v)} placeholder="Add LinkedIn URL" />
                  {lead.linkedin_url && (
                    <a href={lead.linkedin_url} target="_blank" rel="noopener noreferrer" className="ml-1 text-muted-foreground hover:text-foreground">
                      <ExternalLink className="size-3" />
                    </a>
                  )}
                </InfoRow>
                <InfoRow icon={<AtSign className="size-4" />} label="Twitter">
                  <EditableCell value={lead.twitter_url || ""} onSave={(v) => saveField("twitter_url", v)} placeholder="Add Twitter URL" />
                </InfoRow>
              </div>
            </CardContent>
          </Card>

          {/* Company Card */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Building2 className="size-4 text-muted-foreground" /> Company Details
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <InfoRow icon={<Globe className="size-4" />} label="Website">
                  <EditableCell value={lead.website || ""} onSave={(v) => saveField("website", v)} placeholder="Add website" />
                  {lead.website && (
                    <a href={lead.website} target="_blank" rel="noopener noreferrer" className="ml-1 text-muted-foreground hover:text-foreground">
                      <ExternalLink className="size-3" />
                    </a>
                  )}
                </InfoRow>
                <InfoRow icon={<MapPin className="size-4" />} label="Location">
                  <span className="text-sm">{[lead.city, lead.state].filter(Boolean).join(", ") || "—"}</span>
                </InfoRow>
                <InfoRow icon={<Building2 className="size-4" />} label="Size">
                  <EditableCell value={lead.company_size || ""} onSave={(v) => saveField("company_size", v)} placeholder="e.g. 50-200" />
                </InfoRow>
                <InfoRow icon={<Zap className="size-4" />} label="Specialization">
                  <EditableCell value={lead.specialization || ""} onSave={(v) => saveField("specialization", v)} placeholder="Add specialization" />
                </InfoRow>
                {lead.address && (
                  <div className="sm:col-span-2">
                    <InfoRow icon={<MapPin className="size-4" />} label="Address">
                      <span className="text-sm">{lead.address}</span>
                    </InfoRow>
                  </div>
                )}
              </div>

              {/* OpenStreetMap embed */}
              <LeafletMap
                query={[lead.address, lead.city, lead.state].filter(Boolean).join(", ")}
                fallbackQueries={[
                  [lead.city, lead.state].filter(Boolean).join(", "),
                  lead.city || "",
                ]}
                show={!!(lead.city || lead.address)}
                label={lead.company}
              />
            </CardContent>
          </Card>

          {/* Description / AI Research */}
          <Card className={`${TIER_GLOW[tier]} transition-shadow`}>
            <CardHeader className="pb-3">
              <div className="flex items-center justify-between">
                <CardTitle className="text-sm font-medium flex items-center gap-2">
                  <Sparkles className="size-4 text-muted-foreground" /> AI Research
                </CardTitle>
                {lead.last_enriched_at && (
                  <span className="text-xs text-muted-foreground">
                    Last enriched: {new Date(lead.last_enriched_at).toLocaleDateString()}
                  </span>
                )}
              </div>
            </CardHeader>
            <CardContent>
              {(researchContent || lead.description) ? (
                <div className="text-sm leading-relaxed">
                  <MarkdownContent content={researchContent || lead.description} />
                  {enriching === "web_research" && (
                    <span className="inline-block w-2 h-4 bg-foreground/60 animate-pulse ml-0.5" />
                  )}
                </div>
              ) : (
                <p className="text-sm text-muted-foreground italic">
                  No research yet. Click "Research" to generate an AI analysis of this company.
                </p>
              )}
            </CardContent>
          </Card>

          {/* Notes */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Pencil className="size-4 text-muted-foreground" /> Notes
              </CardTitle>
            </CardHeader>
            <CardContent>
              <EditableCell
                value={lead.notes || ""}
                onSave={(v) => saveField("notes", v)}
                placeholder="Add notes about this lead..."
                className="text-sm min-h-[60px]"
              />
            </CardContent>
          </Card>
        </div>

        {/* Right Column — AI Actions + Timeline */}
        <div className="space-y-4">
          {/* AI Enrichment Actions */}
          <Card className="border-dashed">
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Sparkles className="size-4 text-amber-500" /> Enrich with AI
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2">
              <EnrichButton
                icon={<Search className="size-4" />}
                label="Research Company"
                description="AI-powered company analysis"
                loading={enriching === "web_research"}
                disabled={!!enriching}
                onClick={() => handleEnrich("web_research")}
              />
              <EnrichButton
                icon={<Mail className="size-4" />}
                label="Find Emails"
                description="Discover email patterns"
                loading={enriching === "find_emails"}
                disabled={!!enriching}
                onClick={() => handleEnrich("find_emails")}
              />
              <EnrichButton
                icon={<Globe className="size-4" />}
                label="Scrape Website"
                description="Extract data from website"
                loading={enriching === "scrape_website"}
                disabled={!!enriching || !lead.website}
                onClick={() => handleEnrich("scrape_website")}
              />
              <EnrichButton
                icon={<Phone className="size-4" />}
                label="Find Phone"
                description="Search web for contact number"
                loading={enriching === "find_phone"}
                disabled={!!enriching}
                onClick={() => handleEnrich("find_phone")}
              />
              <EnrichButton
                icon={<MapPin className="size-4" />}
                label="Find Address"
                description="OpenStreetMap + web search"
                loading={enriching === "find_address"}
                disabled={!!enriching}
                onClick={() => handleEnrich("find_address")}
              />

              {/* Enrichment Log */}
              {enrichLog.length > 0 && (
                <>
                  <Separator className="my-3" />
                  <div className="space-y-1.5">
                    {enrichLog.map((log, i) => (
                      <div key={i} className="flex items-start gap-2 text-xs">
                        {log.step === "error" ? (
                          <AlertCircle className="size-3 text-destructive shrink-0 mt-0.5" />
                        ) : (
                          <CheckCircle className="size-3 text-emerald-500 shrink-0 mt-0.5" />
                        )}
                        <span className="text-muted-foreground">{log.message}</span>
                      </div>
                    ))}
                  </div>
                </>
              )}
            </CardContent>
          </Card>

          {/* Timeline */}
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Calendar className="size-4 text-muted-foreground" /> Timeline
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3 text-sm">
              <TimelineItem label="Created" date={lead.created_at} />
              <TimelineItem label="Updated" date={lead.updated_at} />
              <TimelineItem label="Last Enriched" date={lead.last_enriched_at} />
              {lead.source && (
                <div className="flex items-center justify-between">
                  <span className="text-muted-foreground">Source</span>
                  <Badge variant="outline" className="text-xs">{lead.source}</Badge>
                </div>
              )}
              {lead.workspace_id && (
                <div className="flex items-center justify-between">
                  <span className="text-muted-foreground">Workspace</span>
                  <span className="text-xs font-mono">{lead.workspace_id}</span>
                </div>
              )}
            </CardContent>
          </Card>

          {/* Value Prop (if exists) */}
          {lead.yupcha_value_prop && (
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-sm font-medium">Value Proposition</CardTitle>
              </CardHeader>
              <CardContent>
                <MarkdownContent content={lead.yupcha_value_prop} className="text-sm text-muted-foreground" />
              </CardContent>
            </Card>
          )}

          {/* Company Need (if exists) */}
          {lead.company_need && (
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-sm font-medium">Company Need</CardTitle>
              </CardHeader>
              <CardContent>
                <MarkdownContent content={lead.company_need} className="text-sm text-muted-foreground" />
              </CardContent>
            </Card>
          )}
        </div>
      </div>
    </div>
  )
}


// ── Sub-components ──────────────────────────────────────────────

function InfoRow({ icon, label, children }: {
  icon: React.ReactNode
  label: string
  children: React.ReactNode
}) {
  return (
    <div className="flex items-center gap-2 min-w-0">
      <div className="text-muted-foreground shrink-0">{icon}</div>
      <div className="text-xs text-muted-foreground w-16 shrink-0">{label}</div>
      <div className="flex items-center gap-1 min-w-0 flex-1">{children}</div>
    </div>
  )
}

function EnrichButton({ icon, label, description, loading, disabled, onClick }: {
  icon: React.ReactNode
  label: string
  description: string
  loading: boolean
  disabled: boolean
  onClick: () => void
}) {
  return (
    <button
      className="w-full flex items-center gap-3 rounded-lg border border-dashed p-3 text-left transition-colors hover:bg-muted/50 disabled:opacity-50 disabled:pointer-events-none"
      onClick={onClick}
      disabled={disabled}
    >
      <div className="shrink-0 text-muted-foreground">
        {loading ? <Loader2 className="size-4 animate-spin" /> : icon}
      </div>
      <div className="min-w-0">
        <div className="text-sm font-medium">{label}</div>
        <div className="text-xs text-muted-foreground">{description}</div>
      </div>
    </button>
  )
}

function TimelineItem({ label, date }: { label: string; date: string | null | undefined }) {
  return (
    <div className="flex items-center justify-between">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-xs">
        {date ? new Date(date).toLocaleDateString("en-US", {
          month: "short", day: "numeric", year: "numeric",
        }) : "—"}
      </span>
    </div>
  )
}
