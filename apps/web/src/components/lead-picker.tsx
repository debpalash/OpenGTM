// Lead picker dialog for sequence enrollment (spec §4).
//
// Wraps `useLeads` with a tier/search filter + checkbox selection and returns
// the chosen lead ids (`number[]`) to the caller. Replaces the brittle
// "fetch by tier then map ids" flow in the old outreach page. The
// `consent_source` is chosen separately next to the Enroll button (see
// SequenceDetailView) — not inside the picker — to keep consent explicit.

import { useMemo, useState } from "react"
import { Search, Loader2, Mail } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Checkbox } from "@/components/ui/checkbox"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useLeads } from "@/lib/hooks"

interface LeadPickerProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called with the selected lead ids when the user confirms. */
  onConfirm: (leadIds: number[]) => void
  /** Disable confirm while an enroll request is in flight. */
  pending?: boolean
}

const TIERS = ["all", "hot", "warm", "cold"] as const

export function LeadPicker({ open, onOpenChange, onConfirm, pending }: LeadPickerProps) {
  const [tier, setTier] = useState<string>("all")
  const [search, setSearch] = useState("")
  const [selected, setSelected] = useState<Set<number>>(new Set())

  const filters = useMemo<Record<string, string>>(() => {
    const f: Record<string, string> = { limit: "200" }
    if (tier !== "all") f.tier = tier
    if (search.trim()) f.search = search.trim()
    return f
  }, [tier, search])

  const { data: leads, isLoading } = useLeads(filters)

  // Only leads with a usable email can be enrolled.
  const rows = useMemo(
    () => (leads ?? []).filter((l) => l.email?.includes("@")),
    [leads],
  )

  const toggle = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const allVisibleSelected = rows.length > 0 && rows.every((l) => selected.has(l.id))
  const toggleAll = () => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (allVisibleSelected) rows.forEach((l) => next.delete(l.id))
      else rows.forEach((l) => next.add(l.id))
      return next
    })
  }

  const confirm = () => {
    onConfirm(Array.from(selected))
    setSelected(new Set())
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>Pick leads to enroll</DialogTitle>
          <DialogDescription>
            Only leads with a valid email are shown. Choose a consent basis after
            confirming.
          </DialogDescription>
        </DialogHeader>

        <div className="flex items-center gap-2">
          <div className="relative flex-1">
            <Search className="absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              aria-label="Search leads"
              placeholder="Search company / email…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="h-8 pl-7 text-xs"
            />
          </div>
          <Select value={tier} onValueChange={(v) => setTier(v ?? "all")}>
            <SelectTrigger className="h-8 w-[120px] text-xs" aria-label="Filter by tier">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {TIERS.map((t) => (
                <SelectItem key={t} value={t} className="text-xs capitalize">
                  {t === "all" ? "All tiers" : `${t} leads`}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        <div className="rounded-md border">
          <div className="flex items-center gap-2 border-b px-3 py-2">
            <Checkbox
              id="lp-select-all"
              checked={allVisibleSelected}
              onCheckedChange={toggleAll}
              disabled={rows.length === 0}
              aria-label="Select all visible leads"
            />
            <Label htmlFor="lp-select-all" className="text-[11px] text-muted-foreground">
              {selected.size > 0 ? `${selected.size} selected` : "Select all"}
            </Label>
          </div>
          <ScrollArea className="h-64">
            {isLoading ? (
              <div className="flex items-center justify-center gap-2 p-8 text-xs text-muted-foreground">
                <Loader2 className="size-4 animate-spin" /> Loading leads…
              </div>
            ) : rows.length === 0 ? (
              <div className="flex flex-col items-center justify-center gap-2 p-8 text-center text-xs text-muted-foreground">
                <Mail className="size-5 text-muted-foreground/40" />
                No leads with emails match this filter.
              </div>
            ) : (
              <ul className="divide-y">
                {rows.map((l) => {
                  const cbId = `lp-lead-${l.id}`
                  return (
                    <li key={l.id} className="flex items-center gap-2 px-3 py-2">
                      <Checkbox
                        id={cbId}
                        checked={selected.has(l.id)}
                        onCheckedChange={() => toggle(l.id)}
                        aria-label={`Select ${l.company || l.email}`}
                      />
                      <Label htmlFor={cbId} className="min-w-0 flex-1 cursor-pointer">
                        <span className="block truncate text-xs font-medium">
                          {l.company || "(no company)"}
                          {l.score_tier && (
                            <span className="ml-1.5 text-[10px] capitalize text-muted-foreground">
                              {l.score_tier}
                            </span>
                          )}
                        </span>
                        <span className="block truncate text-[11px] text-muted-foreground">
                          {l.email}
                        </span>
                      </Label>
                    </li>
                  )
                })}
              </ul>
            )}
          </ScrollArea>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>
            Cancel
          </Button>
          <Button onClick={confirm} disabled={pending || selected.size === 0}>
            {pending ? (
              <Loader2 className="size-3.5 animate-spin" />
            ) : (
              `Enroll ${selected.size || ""}`.trim()
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
