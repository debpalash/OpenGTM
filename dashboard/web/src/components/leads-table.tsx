import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { updateStatus, deleteLead, type Lead } from "@/lib/api"

const TIER_COLORS: Record<string, string> = {
  hot: "bg-red-500/15 text-red-400 border-red-500/30",
  warm: "bg-amber-500/15 text-amber-400 border-amber-500/30",
  cold: "bg-blue-500/15 text-blue-400 border-blue-500/30",
  unqualified: "bg-zinc-500/15 text-zinc-500 border-zinc-500/30",
}

const STATUS_COLORS: Record<string, string> = {
  new: "bg-indigo-500/15 text-indigo-400 border-indigo-500/30",
  contacted: "bg-amber-500/15 text-amber-400 border-amber-500/30",
  qualified: "bg-emerald-500/15 text-emerald-400 border-emerald-500/30",
  negotiating: "bg-purple-500/15 text-purple-400 border-purple-500/30",
  converted: "bg-emerald-500/20 text-emerald-300 border-emerald-400/40",
  dead: "bg-zinc-500/10 text-zinc-600 border-zinc-600/30",
}

const STATUSES = ["new", "contacted", "qualified", "negotiating", "converted", "dead"]

interface Props {
  leads: Lead[]
  onRowClick: (lead: Lead) => void
  onStatusChange: () => void
}

export function LeadsTable({ leads, onRowClick, onStatusChange }: Props) {
  const handleStatus = async (id: number, status: string) => {
    await updateStatus(id, status)
    onStatusChange()
  }

  const handleDelete = async (id: number) => {
    if (!confirm("Delete this lead?")) return
    await deleteLead(id)
    onStatusChange()
  }

  return (
    <Table>
      <TableHeader>
        <TableRow className="border-zinc-800 hover:bg-transparent">
          <TableHead className="w-12 text-[10px] h-7">
            <Tooltip><TooltipTrigger>SCORE</TooltipTrigger><TooltipContent>ICP lead score 0-100</TooltipContent></Tooltip>
          </TableHead>
          <TableHead className="text-[10px] h-7">COMPANY</TableHead>
          <TableHead className="text-[10px] h-7 w-20">CITY</TableHead>
          <TableHead className="text-[10px] h-7">EMAIL</TableHead>
          <TableHead className="text-[10px] h-7 w-28">PHONE</TableHead>
          <TableHead className="text-[10px] h-7 w-36">
            <Tooltip><TooltipTrigger>SPEC</TooltipTrigger><TooltipContent>Company specialization</TooltipContent></Tooltip>
          </TableHead>
          <TableHead className="text-[10px] h-7 w-20">SOURCE</TableHead>
          <TableHead className="text-[10px] h-7 w-24">STATUS</TableHead>
          <TableHead className="text-[10px] h-7 w-12"></TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {leads.length === 0 && (
          <TableRow>
            <TableCell colSpan={9} className="text-center text-zinc-600 h-20 text-xs">No leads found</TableCell>
          </TableRow>
        )}
        {leads.map((lead) => (
          <TableRow
            key={lead.id}
            className="border-zinc-800/50 cursor-pointer hover:bg-zinc-900/80 h-7"
            onClick={() => onRowClick(lead)}
          >
            <TableCell className="py-0.5 px-3">
              <Badge variant="outline" className={`text-[10px] px-1.5 py-0 font-mono font-bold ${TIER_COLORS[lead.score_tier] ?? ""}`}>
                {lead.score}
              </Badge>
            </TableCell>
            <TableCell className="py-0.5 text-xs font-medium truncate max-w-48">{lead.company}</TableCell>
            <TableCell className="py-0.5 text-[11px] text-zinc-400 truncate">{lead.city}</TableCell>
            <TableCell className="py-0.5 text-[11px] truncate max-w-40">
              {lead.email && lead.email !== "N/A" ? (
                <Button variant="link" className="h-auto p-0 text-[11px] text-blue-400" onClick={(e: React.MouseEvent) => { e.stopPropagation(); window.location.href = `mailto:${lead.email}` }}>
                  {lead.email}
                </Button>
              ) : <Badge variant="outline" className="text-[10px] py-0 px-1 text-zinc-700 border-zinc-800">—</Badge>}
            </TableCell>
            <TableCell className="py-0.5 text-[11px] text-zinc-400 truncate">
              {lead.phone && lead.phone !== "N/A" ? lead.phone : <Badge variant="outline" className="text-[10px] py-0 px-1 text-zinc-700 border-zinc-800">—</Badge>}
            </TableCell>
            <TableCell className="py-0.5 text-[10px] text-zinc-500 truncate">{lead.specialization}</TableCell>
            <TableCell className="py-0.5">
              <Badge variant="secondary" className="text-[10px] px-1.5 py-0 bg-zinc-900 text-zinc-500">{lead.source}</Badge>
            </TableCell>
            <TableCell className="py-0.5">
              <Badge variant="outline" className={`text-[10px] px-1.5 py-0 capitalize ${STATUS_COLORS[lead.status] ?? ""}`}>
                {lead.status}
              </Badge>
            </TableCell>
            <TableCell className="py-0.5" onClick={(e: React.MouseEvent) => e.stopPropagation()}>
              <DropdownMenu>
                <DropdownMenuTrigger>
                  <Button variant="ghost" size="sm" className="h-5 w-5 p-0 text-[10px] text-zinc-500">⋯</Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end" className="w-36">
                  {STATUSES.map((s) => (
                    <DropdownMenuItem key={s} className="text-xs capitalize" onClick={() => handleStatus(lead.id, s)}>
                      → {s}
                    </DropdownMenuItem>
                  ))}
                  <DropdownMenuSeparator />
                  <DropdownMenuItem className="text-xs text-red-400" onClick={() => handleDelete(lead.id)}>
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
