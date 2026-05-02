import type { RefObject } from "react"
import type { Filters } from "@/lib/api"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useRef } from "react"

export interface FilterState {
  search: string
  city: string
  tier: string
  status: string
  source: string
  orderBy: string
}

interface Props {
  filters: Filters | null
  state: FilterState
  onChange: (f: FilterState) => void
  searchRef: RefObject<HTMLInputElement | null>
}

export function LeadsFilters({ filters, state, onChange, searchRef }: Props) {
  const timer = useRef<ReturnType<typeof setTimeout>>(null)

  const set = (key: keyof FilterState, val: string | null) => {
    onChange({ ...state, [key]: val ?? "" })
  }

  const handleSearch = (val: string) => {
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => set("search", val), 300)
  }

  return (
    <div className="flex items-center gap-1.5">
      <Input
        ref={searchRef}
        placeholder="Search…"
        defaultValue={state.search}
        onChange={(e) => handleSearch(e.target.value)}
        className="h-7 w-40 text-xs bg-zinc-900 border-zinc-700"
      />

      <Select value={state.city} onValueChange={(v: string | null) => set("city", v)}>
        <SelectTrigger className="h-7 w-24 text-xs bg-zinc-900 border-zinc-700">
          <SelectValue placeholder="City" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="__all__">All Cities</SelectItem>
          {filters?.cities.map((c) => (
            <SelectItem key={c} value={c}>{c}</SelectItem>
          ))}
        </SelectContent>
      </Select>

      <Select value={state.tier} onValueChange={(v: string | null) => set("tier", v)}>
        <SelectTrigger className="h-7 w-24 text-xs bg-zinc-900 border-zinc-700">
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

      <Select value={state.status} onValueChange={(v: string | null) => set("status", v)}>
        <SelectTrigger className="h-7 w-28 text-xs bg-zinc-900 border-zinc-700">
          <SelectValue placeholder="Status" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="__all__">All Statuses</SelectItem>
          {filters?.statuses.map((s) => (
            <SelectItem key={s} value={s}>{s}</SelectItem>
          ))}
        </SelectContent>
      </Select>

      <Select value={state.source} onValueChange={(v: string | null) => set("source", v)}>
        <SelectTrigger className="h-7 w-28 text-xs bg-zinc-900 border-zinc-700">
          <SelectValue placeholder="Source" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="__all__">All Sources</SelectItem>
          {filters?.sources.map((s) => (
            <SelectItem key={s} value={s}>{s}</SelectItem>
          ))}
        </SelectContent>
      </Select>

      <Select value={state.orderBy} onValueChange={(v: string | null) => set("orderBy", v)}>
        <SelectTrigger className="h-7 w-24 text-xs bg-zinc-900 border-zinc-700">
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
  )
}
