import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from "@/components/ui/sheet"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Separator } from "@/components/ui/separator"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { updateStatus, deleteLead, type Lead } from "@/lib/api"

interface Props {
  lead: Lead | null
  open: boolean
  onOpenChange: (open: boolean) => void
  onStatusChange: () => void
  onDelete: () => void
}

const TIER_STYLES: Record<string, string> = {
  hot: "bg-red-500/15 text-red-400 border-red-500/30 score-hot",
  warm: "bg-amber-500/12 text-amber-400 border-amber-500/25",
  cold: "bg-blue-500/10 text-blue-400 border-blue-500/20",
  unqualified: "bg-muted/50 text-muted-foreground border-border",
}

const STATUS_STYLES: Record<string, string> = {
  new: "bg-indigo-500/12 text-indigo-400 border-indigo-500/20",
  contacted: "bg-amber-500/12 text-amber-400 border-amber-500/20",
  qualified: "bg-emerald-500/12 text-emerald-400 border-emerald-500/20",
  negotiating: "bg-purple-500/12 text-purple-400 border-purple-500/20",
  converted: "bg-emerald-500/18 text-emerald-300 border-emerald-400/30",
  dead: "bg-muted/30 text-muted-foreground/50 border-border/50",
}

function isEmpty(val?: string): boolean {
  return !val || val === "N/A" || val === "nan" || val === ""
}

function timeAgo(dateStr: string): string {
  if (!dateStr) return "—"
  const diff = Date.now() - new Date(dateStr).getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  const days = Math.floor(hrs / 24)
  return `${days}d ago`
}

function signals(lead: Lead): { icon: string; label: string; active: boolean }[] {
  return [
    { icon: "📧", label: "Email", active: !isEmpty(lead.email) },
    { icon: "📞", label: "Phone", active: !isEmpty(lead.phone) },
    { icon: "🌐", label: "Website", active: !isEmpty(lead.website) },
    { icon: "🔗", label: "LinkedIn", active: !isEmpty(lead.linkedin_url) },
    { icon: "👤", label: "Contact", active: !isEmpty(lead.contact_person) },
  ]
}

export function LeadDetailSheet({ lead, open, onOpenChange, onStatusChange, onDelete }: Props) {
  if (!lead) return null

  const handleStatus = async (status: string) => {
    await updateStatus(lead.id, status)
    onStatusChange()
  }

  const handleDelete = async () => {
    if (!confirm(`Delete ${lead.company}?`)) return
    await deleteLead(lead.id)
    onDelete()
  }

  const sigs = signals(lead)

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-[400px] sm:w-[440px] bg-background border-border overflow-y-auto p-0">

        {/* ── Header ── */}
        <SheetHeader className="px-5 pt-5 pb-4 border-b border-border">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0 flex-1">
              <SheetTitle className="text-base font-bold text-foreground truncate">{lead.company}</SheetTitle>
              <SheetDescription className="sr-only">Details for {lead.company}</SheetDescription>
              {!isEmpty(lead.city) && (
                <div className="text-[11px] text-muted-foreground mt-0.5">{lead.city}{lead.state ? `, ${lead.state}` : ""}</div>
              )}
            </div>
            <div className="flex items-center gap-1.5 shrink-0 pt-0.5">
              <Badge variant="outline" className={`text-[10px] font-mono font-bold px-2 py-0.5 ${TIER_STYLES[lead.score_tier] ?? TIER_STYLES.unqualified}`}>
                {lead.score}
              </Badge>
              <Badge variant="outline" className={`text-[10px] capitalize px-2 py-0.5 ${STATUS_STYLES[lead.status] ?? ""}`}>
                {lead.status}
              </Badge>
            </div>
          </div>

          {/* Signal indicators */}
          <div className="flex items-center gap-3 mt-3">
            {sigs.map((s) => (
              <Tooltip key={s.label}>
                <TooltipTrigger>
                  <span className={`text-sm transition-opacity ${s.active ? "opacity-100" : "opacity-15"}`}>{s.icon}</span>
                </TooltipTrigger>
                <TooltipContent className="text-xs">{s.label}: {s.active ? "Available" : "Missing"}</TooltipContent>
              </Tooltip>
            ))}
            <span className="text-[10px] text-muted-foreground/40 ml-auto font-mono">{lead.source}</span>
          </div>
        </SheetHeader>

        {/* ── Contact Section ── */}
        <section className="px-5 py-4 space-y-3">
          <SectionLabel>Contact</SectionLabel>
          <div className="grid grid-cols-1 gap-2.5">
            <DataRow
              label="Email"
              value={lead.email}
              action={!isEmpty(lead.email) ? () => window.location.href = `mailto:${lead.email}` : undefined}
              actionLabel="Send"
            />
            <DataRow
              label="Phone"
              value={lead.phone}
              action={!isEmpty(lead.phone) ? () => window.location.href = `tel:${lead.phone}` : undefined}
              actionLabel="Call"
              mono
            />
            <DataRow
              label="Website"
              value={lead.website}
              action={!isEmpty(lead.website) ? () => window.open(lead.website, "_blank") : undefined}
              actionLabel="Open"
              truncateValue
            />
          </div>
        </section>

        <Separator />

        {/* ── Company Details ── */}
        <section className="px-5 py-4 space-y-3">
          <SectionLabel>Company</SectionLabel>
          <div className="grid grid-cols-2 gap-x-4 gap-y-2.5">
            <DataField label="Specialization" value={lead.specialization} />
            <DataField label="Company Size" value={lead.company_size} />
            <DataField label="Contact Person" value={lead.contact_person} />
            <DataField label="Contact Title" value={lead.contact_title} />
          </div>
          {!isEmpty(lead.description) && (
            <div className="mt-2">
              <div className="text-[10px] text-muted-foreground/50 uppercase tracking-wider font-medium mb-1">Description</div>
              <p className="text-[11px] text-muted-foreground leading-relaxed">{lead.description}</p>
            </div>
          )}
        </section>

        <Separator />

        {/* ── Social & Links ── */}
        <section className="px-5 py-4 space-y-3">
          <SectionLabel>Links</SectionLabel>
          <div className="flex gap-2">
            <LinkButton label="LinkedIn" url={lead.linkedin_url} />
            <LinkButton label="Twitter" url={lead.twitter_url} />
          </div>
        </section>

        {/* ── Notes (only if data exists) ── */}
        {(!isEmpty(lead.notes) || !isEmpty(lead.yupcha_value_prop) || !isEmpty(lead.company_need)) && (
          <>
            <Separator />
            <section className="px-5 py-4 space-y-2.5">
              <SectionLabel>Notes & Context</SectionLabel>
              {!isEmpty(lead.yupcha_value_prop) && <DataField label="Value Proposition" value={lead.yupcha_value_prop} />}
              {!isEmpty(lead.company_need) && <DataField label="Company Need" value={lead.company_need} />}
              {!isEmpty(lead.notes) && <DataField label="Notes" value={lead.notes} />}
            </section>
          </>
        )}

        <Separator />

        {/* ── Timeline ── */}
        <section className="px-5 py-4 space-y-2">
          <SectionLabel>Timeline</SectionLabel>
          <div className="flex items-center justify-between text-[10px]">
            <span className="text-muted-foreground/40">Created</span>
            <span className="text-muted-foreground font-mono">{timeAgo(lead.created_at)}</span>
          </div>
          <div className="flex items-center justify-between text-[10px]">
            <span className="text-muted-foreground/40">Updated</span>
            <span className="text-muted-foreground font-mono">{timeAgo(lead.updated_at)}</span>
          </div>
          {!isEmpty(lead.last_enriched_at) && (
            <div className="flex items-center justify-between text-[10px]">
              <span className="text-muted-foreground/40">Enriched</span>
              <span className="text-muted-foreground font-mono">{timeAgo(lead.last_enriched_at)}</span>
            </div>
          )}
        </section>

        <Separator />

        {/* ── Status Actions ── */}
        <section className="px-5 py-4 space-y-2.5">
          <SectionLabel>Change Status</SectionLabel>
          <div className="flex flex-wrap gap-1.5">
            {(["new", "contacted", "qualified", "negotiating", "converted", "dead"] as const).map((s) => (
              <Button
                key={s}
                variant={lead.status === s ? "default" : "outline"}
                size="sm"
                className={`h-7 text-[11px] capitalize px-3 ${
                  lead.status === s ? "" : "border-border/60 text-muted-foreground hover:text-foreground"
                }`}
                onClick={() => handleStatus(s)}
              >
                {s}
              </Button>
            ))}
          </div>
        </section>

        {/* ── Delete ── */}
        <div className="px-5 pb-5 pt-1">
          <Button
            variant="ghost"
            size="sm"
            className="w-full h-8 text-[11px] text-destructive/60 hover:text-destructive hover:bg-destructive/10 border border-border/30"
            onClick={handleDelete}
          >
            Delete Lead
          </Button>
        </div>

      </SheetContent>
    </Sheet>
  )
}

/* ── Sub-components ── */

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="text-[10px] text-muted-foreground/40 uppercase tracking-widest font-semibold">{children}</div>
  )
}

function DataField({ label, value }: { label: string; value?: string }) {
  return (
    <div>
      <div className="text-[10px] text-muted-foreground/40 uppercase tracking-wider font-medium mb-0.5">{label}</div>
      <div className={`text-[12px] ${isEmpty(value) ? "text-muted-foreground/15" : "text-foreground/80"}`}>
        {isEmpty(value) ? "—" : value}
      </div>
    </div>
  )
}

function DataRow({ label, value, action, actionLabel, mono, truncateValue }: {
  label: string
  value?: string
  action?: () => void
  actionLabel?: string
  mono?: boolean
  truncateValue?: boolean
}) {
  const empty = isEmpty(value)
  return (
    <div className="flex items-center justify-between gap-3 py-1.5 px-3 rounded-lg bg-card/50 border border-border/30">
      <div className="min-w-0 flex-1">
        <div className="text-[9px] text-muted-foreground/40 uppercase tracking-wider font-medium">{label}</div>
        <div className={`text-[12px] ${empty ? "text-muted-foreground/15" : "text-foreground/80"} ${mono ? "font-mono" : ""} ${truncateValue ? "truncate" : "break-all"}`}>
          {empty ? "—" : value}
        </div>
      </div>
      {action && !empty && (
        <Button
          variant="ghost"
          size="sm"
          className="h-6 text-[10px] text-primary/70 hover:text-primary px-2 shrink-0"
          onClick={(e: React.MouseEvent) => { e.stopPropagation(); action() }}
        >
          {actionLabel}
        </Button>
      )}
    </div>
  )
}

function LinkButton({ label, url }: { label: string; url?: string }) {
  const active = !isEmpty(url)
  return (
    <Button
      variant="outline"
      size="sm"
      className={`h-7 text-[11px] px-3 flex-1 ${
        active
          ? "border-primary/20 text-primary hover:bg-primary/10"
          : "border-border/30 text-muted-foreground/20 pointer-events-none"
      }`}
      onClick={() => active && window.open(url, "_blank")}
    >
      {label} {active ? "↗" : ""}
    </Button>
  )
}
