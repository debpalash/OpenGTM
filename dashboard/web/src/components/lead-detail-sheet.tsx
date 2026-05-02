import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from "@/components/ui/sheet"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Separator } from "@/components/ui/separator"
import { Card, CardContent } from "@/components/ui/card"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { updateStatus, deleteLead, type Lead } from "@/lib/api"

interface Props {
  lead: Lead | null
  open: boolean
  onOpenChange: (open: boolean) => void
  onStatusChange: () => void
  onDelete: () => void
}

function Field({ label, value, href }: { label: string; value?: string; href?: string }) {
  const empty = !value || value === "N/A" || value === "nan"
  return (
    <div className="space-y-0.5">
      <div className="text-[10px] text-zinc-500 uppercase tracking-wider font-medium">{label}</div>
      {empty ? (
        <Badge variant="outline" className="text-[10px] py-0 px-1 text-zinc-700 border-zinc-800">—</Badge>
      ) : href ? (
        <Button variant="link" className="h-auto p-0 text-xs text-blue-400 break-all text-left justify-start" onClick={() => window.open(href, "_blank")}>
          {value}
        </Button>
      ) : (
        <div className="text-xs text-zinc-300 break-all">{value}</div>
      )}
    </div>
  )
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

  const tierColor: Record<string, string> = {
    hot: "bg-red-500/15 text-red-400 border-red-500/30",
    warm: "bg-amber-500/15 text-amber-400 border-amber-500/30",
    cold: "bg-blue-500/15 text-blue-400 border-blue-500/30",
    unqualified: "bg-zinc-500/15 text-zinc-500 border-zinc-500/30",
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-[380px] bg-zinc-950 border-zinc-800 overflow-y-auto">
        <SheetHeader className="pb-3">
          <SheetTitle className="text-sm font-bold text-zinc-100">{lead.company}</SheetTitle>
          <SheetDescription className="sr-only">Details for {lead.company}</SheetDescription>
          <div className="flex items-center gap-2 mt-1">
            <Badge variant="outline" className={`text-[10px] font-mono font-bold ${tierColor[lead.score_tier] ?? ""}`}>
              {lead.score} · {lead.score_tier}
            </Badge>
            <Badge variant="outline" className="text-[10px] capitalize">{lead.status}</Badge>
          </div>
        </SheetHeader>

        <Separator className="my-3" />

        <Card className="bg-zinc-900/50 border-zinc-800">
          <CardContent className="grid grid-cols-2 gap-3 p-3">
            <Field label="City" value={lead.city} />
            <Field label="Specialization" value={lead.specialization} />
            <Field label="Website" value={lead.website} href={lead.website} />
            <Field label="Email" value={lead.email} href={lead.email ? `mailto:${lead.email}` : undefined} />
            <Field label="Phone" value={lead.phone} href={lead.phone ? `tel:${lead.phone}` : undefined} />
            <Field label="Company Size" value={lead.company_size} />
          </CardContent>
        </Card>

        <Separator className="my-3" />

        <Card className="bg-zinc-900/50 border-zinc-800">
          <CardContent className="grid grid-cols-2 gap-3 p-3">
            <Field label="LinkedIn" value={lead.linkedin_url ? "View Profile" : undefined} href={lead.linkedin_url} />
            <Field label="Twitter" value={lead.twitter_url ? "View Profile" : undefined} href={lead.twitter_url} />
            <Field label="Contact Person" value={lead.contact_person} />
            <Field label="Contact Title" value={lead.contact_title} />
            <Field label="Source" value={lead.source} />
            <Field label="Last Enriched" value={lead.last_enriched_at?.split("T")[0]} />
          </CardContent>
        </Card>

        <Separator className="my-3" />

        <Card className="bg-zinc-900/50 border-zinc-800">
          <CardContent className="space-y-2 p-3">
            <Field label="Value Proposition" value={lead.yupcha_value_prop} />
            <Field label="Company Need" value={lead.company_need} />
            <Field label="Notes" value={lead.notes} />
            <Field label="Description" value={lead.description} />
          </CardContent>
        </Card>

        <Separator className="my-3" />

        <div className="space-y-2">
          <div className="text-[10px] text-zinc-500 uppercase tracking-wider font-medium">Change Status</div>
          <div className="flex flex-wrap gap-1">
            {["new", "contacted", "qualified", "negotiating", "converted", "dead"].map((s) => (
              <Tooltip key={s}>
                <TooltipTrigger>
                  <Button
                    variant={lead.status === s ? "default" : "outline"}
                    size="sm"
                    className="h-6 text-[10px] capitalize"
                    onClick={() => handleStatus(s)}
                  >
                    {s}
                  </Button>
                </TooltipTrigger>
                <TooltipContent>Set status to {s}</TooltipContent>
              </Tooltip>
            ))}
          </div>
        </div>

        <Separator className="my-3" />

        <Button variant="destructive" size="sm" className="w-full h-7 text-xs" onClick={handleDelete}>
          Delete Lead
        </Button>
      </SheetContent>
    </Sheet>
  )
}
