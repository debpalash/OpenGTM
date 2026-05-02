import type { Stats } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

interface Props {
  stats: Stats
}

export function StatsStrip({ stats }: Props) {
  const e = stats.enrichment
  const pct = (n: number) => e.total ? Math.round((n / e.total) * 100) : 0

  return (
    <div className="flex items-center gap-0 px-4 h-7 border-b border-zinc-800 text-[11px] font-medium shrink-0 bg-zinc-900/50">
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="secondary" className="text-[10px] h-4 bg-zinc-800 text-zinc-300">{stats.total} total</Badge>
        </TooltipTrigger>
        <TooltipContent>Total leads in database</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="outline" className="text-[10px] h-4 bg-red-500/10 text-red-400 border-red-500/30">🔥 {stats.by_tier?.hot ?? 0}</Badge>
        </TooltipTrigger>
        <TooltipContent>Hot leads (score 75-100)</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="outline" className="text-[10px] h-4 bg-amber-500/10 text-amber-400 border-amber-500/30">🟡 {stats.by_tier?.warm ?? 0}</Badge>
        </TooltipTrigger>
        <TooltipContent>Warm leads (score 50-74)</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="outline" className="text-[10px] h-4 bg-blue-500/10 text-blue-400 border-blue-500/30">🔵 {stats.by_tier?.cold ?? 0}</Badge>
        </TooltipTrigger>
        <TooltipContent>Cold leads (score 25-49)</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary" className="text-[10px] h-4 bg-zinc-800 text-zinc-400">Avg {Math.round(e.avg_score ?? 0)}</Badge>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary" className="text-[10px] h-4 bg-zinc-800/50 text-zinc-500">📧 {pct(e.with_email)}%</Badge>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary" className="text-[10px] h-4 bg-zinc-800/50 text-zinc-500">📞 {pct(e.with_phone)}%</Badge>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary" className="text-[10px] h-4 bg-zinc-800/50 text-zinc-500">🔗 {pct(e.with_linkedin)}%</Badge>
    </div>
  )
}
