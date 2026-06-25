import { useNavigate } from "react-router-dom"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { updateStatus, deleteLead, type Lead } from "@/lib/api"

const TIER_STYLES: Record<string, { bg: string; text: string; glow?: string }> = {
  hot: { bg: "bg-red-500/15", text: "text-red-400", glow: "score-hot" },
  warm: { bg: "bg-amber-500/12", text: "text-amber-400", glow: "score-warm" },
  cold: { bg: "bg-blue-500/10", text: "text-blue-400" },
  unqualified: { bg: "bg-muted/50", text: "text-muted-foreground" },
}

const STATUS_STYLES: Record<string, string> = {
  new: "bg-indigo-500/12 text-indigo-400 border-indigo-500/20",
  contacted: "bg-amber-500/12 text-amber-400 border-amber-500/20",
  qualified: "bg-emerald-500/12 text-emerald-400 border-emerald-500/20",
  negotiating: "bg-purple-500/12 text-purple-400 border-purple-500/20",
  converted: "bg-emerald-500/18 text-emerald-300 border-emerald-400/30",
  dead: "bg-muted/30 text-muted-foreground/50 border-muted",
}

const STATUSES = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]

interface Props {
  leads: Lead[]
  onRowClick: (lead: Lead) => void
  onStatusChange: () => void
}

function timeAgo(dateStr: string): string {
  if (!dateStr) return "—"
  const diff = Date.now() - new Date(dateStr).getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h`
  const days = Math.floor(hrs / 24)
  return `${days}d`
}

function liveness(lead: Lead): { label: string; color: string; icon: string } {
  if (lead.status === "dead") return { label: "Dead", color: "text-red-400/50", icon: "🔴" }
  if (lead.status === "converted") return { label: "Won", color: "text-emerald-400", icon: "🟢" }

  const hasData = (lead.email && lead.email !== "N/A") || (lead.phone && lead.phone !== "N/A")
  if (hasData && lead.score >= 50) return { label: "Active", color: "text-emerald-400", icon: "🟢" }
  if (hasData) return { label: "Warm", color: "text-amber-400", icon: "🟡" }
  if (lead.website && lead.website !== "N/A") return { label: "Stale", color: "text-amber-400/60", icon: "🟡" }
  return { label: "Cold", color: "text-muted-foreground/40", icon: "⚪" }
}

function signals(lead: Lead): string[] {
  const s: string[] = []
  if (lead.email && lead.email !== "N/A") s.push("📧")
  if (lead.phone && lead.phone !== "N/A") s.push("📞")
  if (lead.linkedin_url && lead.linkedin_url !== "N/A") s.push("🔗")
  if (lead.website && lead.website !== "N/A") s.push("🌐")
  if (lead.contact_person && lead.contact_person !== "N/A") s.push("👤")
  return s
}

export function LeadsTable({ leads, onRowClick, onStatusChange }: Props) {
  const navigate = useNavigate()
  const handleStatus = async (id: number, status: string) => {
    await updateStatus(id, status)
    onStatusChange()
  }

  const handleDelete = async (id: number) => {
    if (!confirm("Delete this lead?")) return
    await deleteLead(id)
    onStatusChange()
  }

  const EmptyCell = () => <span className="text-muted-foreground/15">—</span>

  return (
    <Table>
      <TableHeader>
        <TableRow className="border-border/50 hover:bg-transparent">
          <TableHead className="w-14 text-[10px] h-8 font-semibold uppercase tracking-wider text-muted-foreground/60">Score</TableHead>
          <TableHead className="text-[10px] h-8 font-semibold uppercase tracking-wider text-muted-foreground/60">Company</TableHead>
          <TableHead className="text-[10px] h-8 w-20 font-semibold uppercase tracking-wider text-muted-foreground/60">City</TableHead>
          <TableHead className="text-[10px] h-8 font-semibold uppercase tracking-wider text-muted-foreground/60">Email</TableHead>
          <TableHead className="text-[10px] h-8 w-28 font-semibold uppercase tracking-wider text-muted-foreground/60">Phone</TableHead>
          <TableHead className="text-[10px] h-8 w-14 font-semibold uppercase tracking-wider text-muted-foreground/60">Live</TableHead>
          <TableHead className="text-[10px] h-8 w-20 font-semibold uppercase tracking-wider text-muted-foreground/60">Signals</TableHead>
          <TableHead className="text-[10px] h-8 w-24 font-semibold uppercase tracking-wider text-muted-foreground/60">Source</TableHead>
          <TableHead className="text-[10px] h-8 w-24 font-semibold uppercase tracking-wider text-muted-foreground/60">Status</TableHead>
          <TableHead className="text-[10px] h-8 w-14 font-semibold uppercase tracking-wider text-muted-foreground/60">Age</TableHead>
          <TableHead className="text-[10px] h-8 w-8" />
        </TableRow>
      </TableHeader>
      <TableBody>
        {leads.length === 0 && (
          <TableRow>
            <TableCell colSpan={11} className="text-center text-muted-foreground/40 h-24 text-xs">
              <div className="text-xl mb-1">📋</div>
              No leads match your filters
            </TableCell>
          </TableRow>
        )}
        {leads.map((lead) => {
          const ts = TIER_STYLES[lead.score_tier] || TIER_STYLES.unqualified
          const live = liveness(lead)
          const sigs = signals(lead)
          return (
            <TableRow
              key={lead.id}
              className={`border-border/30 cursor-pointer hover:bg-accent/40 transition-colors h-8 group ${
                lead.status === "dead" ? "opacity-40" : ""
              }`}
              onClick={() => onRowClick(lead)}
            >
              {/* Score */}
              <TableCell className="py-0.5 px-3">
                <Tooltip>
                  <TooltipTrigger>
                    <div className={`inline-flex items-center justify-center w-8 h-5 rounded-md text-[11px] font-bold font-mono ${ts.bg} ${ts.text} ${ts.glow || ""}`}>
                      {lead.score}
                    </div>
                  </TooltipTrigger>
                  <TooltipContent>{lead.score_tier} tier · {lead.score}/100</TooltipContent>
                </Tooltip>
              </TableCell>

              {/* Company */}
              <TableCell className="py-0.5 text-[12px] font-medium truncate max-w-52 text-foreground/90">{lead.company}</TableCell>

              {/* City */}
              <TableCell className="py-0.5 text-[11px] text-muted-foreground truncate">{lead.city || <EmptyCell />}</TableCell>

              {/* Email */}
              <TableCell className="py-0.5 text-[11px] truncate max-w-40">
                {lead.email && lead.email !== "N/A" ? (
                  <button
                    className="text-primary hover:underline text-left truncate block max-w-full"
                    onClick={(e) => { e.stopPropagation(); window.location.href = `mailto:${lead.email}` }}
                  >
                    {lead.email}
                  </button>
                ) : <EmptyCell />}
              </TableCell>

              {/* Phone */}
              <TableCell className="py-0.5 text-[11px] text-muted-foreground truncate font-mono">
                {lead.phone && lead.phone !== "N/A" ? lead.phone : <EmptyCell />}
              </TableCell>

              {/* Liveness */}
              <TableCell className="py-0.5">
                <Tooltip>
                  <TooltipTrigger>
                    <span className={`text-[10px] ${live.color}`}>{live.icon}</span>
                  </TooltipTrigger>
                  <TooltipContent>{live.label}</TooltipContent>
                </Tooltip>
              </TableCell>

              {/* Signals */}
              <TableCell className="py-0.5 text-[10px] tracking-tight">
                {sigs.length > 0 ? sigs.join("") : <EmptyCell />}
              </TableCell>

              {/* Source — clickable for pipeline jobs */}
              <TableCell className="py-0.5">
                {lead.source.startsWith("job:") ? (
                  <button
                    className="text-[10px] text-primary/60 hover:text-primary font-mono truncate block max-w-20 hover:underline"
                    onClick={(e) => { e.stopPropagation(); navigate(`/pipeline/${lead.source.replace('job:', '')}`) }}
                  >
                    {lead.source.slice(0, 12)}
                  </button>
                ) : (
                  <span className="text-[10px] text-muted-foreground/40 font-mono truncate block max-w-20">
                    {lead.source}
                  </span>
                )}
              </TableCell>

              {/* Status */}
              <TableCell className="py-0.5">
                <Badge variant="outline" className={`text-[10px] px-1.5 py-0 capitalize ${STATUS_STYLES[lead.status] ?? ""}`}>
                  {lead.status}
                </Badge>
              </TableCell>

              {/* Age */}
              <TableCell className="py-0.5 text-[10px] text-muted-foreground/40 font-mono">
                {timeAgo(lead.created_at)}
              </TableCell>

              {/* Actions */}
              <TableCell className="py-0.5 opacity-0 group-hover:opacity-100 transition-opacity" onClick={(e: React.MouseEvent) => e.stopPropagation()}>
                <DropdownMenu>
                  <DropdownMenuTrigger>
                    <span className="inline-flex items-center justify-center rounded-md h-5 w-5 text-[10px] text-muted-foreground hover:bg-muted transition-colors cursor-pointer">⋯</span>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end" className="w-36">
                    {STATUSES.map((s) => (
                      <DropdownMenuItem key={s} className="text-xs capitalize" onClick={() => handleStatus(lead.id, s)}>
                        → {s}
                      </DropdownMenuItem>
                    ))}
                    <DropdownMenuSeparator />
                    <DropdownMenuItem className="text-xs text-destructive" onClick={() => handleDelete(lead.id)}>
                      Delete
                    </DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
