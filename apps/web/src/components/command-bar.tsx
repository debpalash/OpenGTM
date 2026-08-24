import { useState, type RefObject } from "react"
import type { Filters } from "@/lib/api"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"

interface Props {
  filters: Filters | null
  search: string
  city: string
  tier: string
  status: string
  source: string
  orderBy: string
  onSearchChange: (v: string) => void
  onCityChange: (v: string) => void
  onTierChange: (v: string) => void
  onStatusChange: (v: string) => void
  onSourceChange: (v: string) => void
  onOrderByChange: (v: string) => void
  onCollect: (query: string) => void
  onExport: () => void
  onAdd: () => void
  searchRef: RefObject<HTMLInputElement | null>
  view: string
  onViewChange: (v: "leads" | "pipeline") => void
}

export function CommandBar(props: Props) {
  const [collectQuery, setCollectQuery] = useState("")
  const [collectOpen, setCollectOpen] = useState(false)
  const [searchTimer, setSearchTimer] = useState<ReturnType<typeof setTimeout> | null>(null)

  const handleSearch = (val: string) => {
    if (searchTimer) clearTimeout(searchTimer)
    setSearchTimer(setTimeout(() => props.onSearchChange(val), 300))
  }

  const handleCollectSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!collectQuery.trim()) return
    props.onCollect(collectQuery.trim())
    setCollectQuery("")
    setCollectOpen(false)
  }

  const clean = (v: string | null) => (v === "__all__" ? "" : v ?? "")

  return (
    <div className="shrink-0 border-b border-border bg-card/80 backdrop-blur-sm">
      {/* ── Top Row: Brand + Actions ── */}
      <div className="flex items-center justify-between px-4 h-11">
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-2">
            <div className="w-2 h-2 rounded-full bg-primary live-dot" />
            <span className="text-sm font-semibold tracking-tight">OpenGTM</span>
            <Badge variant="outline" className="text-[9px] h-4 px-1.5 border-border text-muted-foreground font-medium">
              Pipeline
            </Badge>
          </div>
          <Separator orientation="vertical" className="h-4" />
          <div className="flex items-center gap-0.5">
            <Button
              variant={props.view === "leads" ? "secondary" : "ghost"}
              size="sm"
              className="h-6 text-[11px] px-2.5 rounded-full"
              onClick={() => props.onViewChange("leads")}
            >
              Leads
            </Button>
            <Button
              variant={props.view === "pipeline" ? "secondary" : "ghost"}
              size="sm"
              className="h-6 text-[11px] px-2.5 rounded-full"
              onClick={() => props.onViewChange("pipeline")}
            >
              Pipeline
            </Button>
          </div>
        </div>

        <div className="flex items-center gap-1.5">
          <Button
            variant={collectOpen ? "default" : "outline"}
            size="sm"
            className="h-7 text-[11px] gap-1.5"
            onClick={() => setCollectOpen(!collectOpen)}
          >
            <svg className="w-3 h-3" viewBox="0 0 16 16" fill="none"><path d="M14 14L10.5 10.5M11.5 6.5C11.5 9.26 9.26 11.5 6.5 11.5C3.74 11.5 1.5 9.26 1.5 6.5C1.5 3.74 3.74 1.5 6.5 1.5C9.26 1.5 11.5 3.74 11.5 6.5Z" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"/></svg>
            Collect
          </Button>
          <Button variant="ghost" size="sm" className="h-7 text-[11px]" onClick={props.onExport}>
            Export
          </Button>
          <Button size="sm" className="h-7 text-[11px]" onClick={props.onAdd}>
            + Add
          </Button>
        </div>
      </div>

      {/* ── Collect Input (expandable) ── */}
      {collectOpen && (
        <div className="px-4 pb-2.5">
          <form onSubmit={handleCollectSubmit} className="flex gap-2">
            <Input
              autoFocus
              placeholder="Enter a query, e.g. 'IT staffing companies Mumbai'…"
              value={collectQuery}
              onChange={(e) => setCollectQuery(e.target.value)}
              className="h-8 text-xs flex-1 bg-background/50"
            />
            <Button type="submit" size="sm" className="h-8 text-[11px] px-4" disabled={!collectQuery.trim()}>
              Start Collection
            </Button>
          </form>
        </div>
      )}

      {/* ── Filter Row ── */}
      <div className="flex items-center gap-1.5 px-4 h-9 border-t border-border/50">
        <Input
          ref={props.searchRef}
          placeholder="Search leads…"
          defaultValue={props.search}
          onChange={(e) => handleSearch(e.target.value)}
          className="h-6 w-44 text-[11px] bg-background/30 border-border/50 rounded-md"
        />

        <Select value={props.city || "__all__"} onValueChange={(v) => props.onCityChange(clean(v))}>
          <SelectTrigger className="h-6 w-24 text-[11px] bg-background/30 border-border/50">
            <SelectValue placeholder="City" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all__">All Cities</SelectItem>
            {props.filters?.cities.map((c) => (
              <SelectItem key={c} value={c}>{c}</SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={props.tier || "__all__"} onValueChange={(v) => props.onTierChange(clean(v))}>
          <SelectTrigger className="h-6 w-24 text-[11px] bg-background/30 border-border/50">
            <SelectValue placeholder="Tier" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all__">All Tiers</SelectItem>
            <SelectItem value="hot">🔥 Hot</SelectItem>
            <SelectItem value="warm">🟡 Warm</SelectItem>
            <SelectItem value="cold">🔵 Cold</SelectItem>
            <SelectItem value="unqualified">⚪ Unq</SelectItem>
          </SelectContent>
        </Select>

        <Select value={props.status || "__all__"} onValueChange={(v) => props.onStatusChange(clean(v))}>
          <SelectTrigger className="h-6 w-28 text-[11px] bg-background/30 border-border/50">
            <SelectValue placeholder="Status" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all__">All Statuses</SelectItem>
            {props.filters?.statuses.map((s) => (
              <SelectItem key={s} value={s}>{s}</SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={props.source || "__all__"} onValueChange={(v) => props.onSourceChange(clean(v))}>
          <SelectTrigger className="h-6 w-28 text-[11px] bg-background/30 border-border/50">
            <SelectValue placeholder="Source" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all__">All Sources</SelectItem>
            {props.filters?.sources.map((s) => (
              <SelectItem key={s} value={s}>{s}</SelectItem>
            ))}
          </SelectContent>
        </Select>

        <div className="flex-1" />

        <Select value={props.orderBy} onValueChange={(v) => v && props.onOrderByChange(v)}>
          <SelectTrigger className="h-6 w-24 text-[11px] bg-background/30 border-border/50">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="score DESC">Score ↓</SelectItem>
            <SelectItem value="score ASC">Score ↑</SelectItem>
            <SelectItem value="company ASC">Name A-Z</SelectItem>
            <SelectItem value="created_at DESC">Newest</SelectItem>
            <SelectItem value="city ASC">City</SelectItem>
          </SelectContent>
        </Select>
      </div>
    </div>
  )
}
