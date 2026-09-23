import type { Stats } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { ChannelIcon, TierIcon } from "@/components/semantic-icons"

interface Props {
  stats: Stats
}

export function StatsStrip({ stats }: Props) {
  const e = stats.enrichment
  const pct = (n: number) => e.total ? Math.round((n / e.total) * 100) : 0

  return (
    <div className="flex items-center gap-0 px-4 h-8 border-b border-border text-xs font-medium shrink-0 bg-[var(--t-background-secondary)]">
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="secondary" className="text-foreground">{stats.total} total</Badge>
        </TooltipTrigger>
        <TooltipContent>Total leads in database</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="outline"><TierIcon tier="hot" className="size-3" />{stats.by_tier?.hot ?? 0}<span className="sr-only"> hot</span></Badge>
        </TooltipTrigger>
        <TooltipContent>Hot leads (score 75-100)</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="outline"><TierIcon tier="warm" className="size-3" />{stats.by_tier?.warm ?? 0}<span className="sr-only"> warm</span></Badge>
        </TooltipTrigger>
        <TooltipContent>Warm leads (score 50-74)</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Tooltip>
        <TooltipTrigger>
          <Badge variant="outline"><TierIcon tier="cold" className="size-3" />{stats.by_tier?.cold ?? 0}<span className="sr-only"> cold</span></Badge>
        </TooltipTrigger>
        <TooltipContent>Cold leads (score 25-49)</TooltipContent>
      </Tooltip>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary">Avg {Math.round(e.avg_score ?? 0)}</Badge>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary"><ChannelIcon channel="email" className="size-3" />{pct(e.with_email)}%<span className="sr-only"> with email</span></Badge>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary"><ChannelIcon channel="phone" className="size-3" />{pct(e.with_phone)}%<span className="sr-only"> with phone</span></Badge>
      <Separator orientation="vertical" className="mx-2 h-3" />
      <Badge variant="secondary"><ChannelIcon channel="linkedin" className="size-3" />{pct(e.with_linkedin)}%<span className="sr-only"> with LinkedIn</span></Badge>
    </div>
  )
}
