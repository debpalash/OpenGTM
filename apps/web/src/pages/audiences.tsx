import { useEffect, useState } from "react"
import { Activity, ArrowDownLeft, ArrowUpRight, ListFilter, RefreshCw, Users } from "lucide-react"
import { toast } from "sonner"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { useAudienceEvents, useAudienceMembers, useAudiences, useRefreshAudience, useUpdateAudience } from "@/lib/hooks"

const ago = (value: string | null) => {
  if (!value) return "Never"
  const minutes = Math.max(0, Math.round((Date.now() - new Date(value).getTime()) / 60_000))
  if (minutes < 1) return "Just now"
  if (minutes < 60) return `${minutes}m ago`
  if (minutes < 1440) return `${Math.round(minutes / 60)}h ago`
  return `${Math.round(minutes / 1440)}d ago`
}

export default function AudiencesPage() {
  const audiences = useAudiences()
  const [selectedId, setSelectedId] = useState<string | null>(null)
  useEffect(() => {
    if (!selectedId && audiences.data?.length) setSelectedId(audiences.data[0].id)
  }, [audiences.data, selectedId])
  const selected = audiences.data?.find((audience) => audience.id === selectedId)
  const members = useAudienceMembers(selectedId)
  const events = useAudienceEvents(selectedId)
  const refresh = useRefreshAudience()
  const update = useUpdateAudience()

  const runRefresh = async () => {
    if (!selectedId) return
    try {
      const result = await refresh.mutateAsync(selectedId)
      toast.success(`Audience refreshed: +${result.entered} entered, −${result.exited} exited`)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Could not refresh audience")
    }
  }

  return (
    <div className="flex h-full min-h-0 gap-4 p-4">
      <aside className="w-72 shrink-0 space-y-3 overflow-y-auto">
        <div>
          <h1 className="text-xl font-semibold">Audiences</h1>
          <p className="text-xs text-muted-foreground">Dynamic, workspace-wide lead segments</p>
        </div>
        {audiences.isLoading && <Skeleton className="h-24 w-full" />}
        {audiences.data?.map((audience) => (
          <button
            key={audience.id}
            onClick={() => setSelectedId(audience.id)}
            className={`w-full rounded-lg border p-3 text-left transition-colors ${selectedId === audience.id ? "border-primary bg-primary/5" : "hover:bg-muted/50"}`}
          >
            <div className="flex items-center justify-between gap-2">
              <span className="truncate text-sm font-medium">{audience.name}</span>
              <Badge variant="secondary">{audience.member_count}</Badge>
            </div>
            <p className="mt-2 text-[11px] text-muted-foreground">Refreshed {ago(audience.refreshed_at)}</p>
          </button>
        ))}
        {!audiences.isLoading && !audiences.data?.length && (
          <Card><CardContent className="p-4 text-xs text-muted-foreground">Create an audience from filtered Leads.</CardContent></Card>
        )}
      </aside>

      <main className="min-w-0 flex-1 overflow-y-auto">
        {!selected ? (
          <div className="grid h-full place-items-center text-sm text-muted-foreground"><ListFilter className="mr-2 size-4" /> Select an audience</div>
        ) : (
          <div className="space-y-4">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div>
                <h2 className="text-lg font-semibold">{selected.name}</h2>
                <p className="text-xs text-muted-foreground">{Object.keys(selected.filters).length} active filters · {selected.member_count} current members</p>
              </div>
              <div className="flex items-center gap-2">
                <select
                  aria-label="Audience refresh interval"
                  value={selected.refresh_interval_minutes}
                  disabled={!selected.refresh_enabled || update.isPending}
                  onChange={(event) => update.mutate({ id: selected.id, data: { refresh_interval_minutes: Number(event.target.value) } })}
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                >
                  <option value={15}>Every 15 minutes</option>
                  <option value={60}>Hourly</option>
                  <option value={360}>Every 6 hours</option>
                  <option value={1440}>Daily</option>
                  <option value={10080}>Weekly</option>
                </select>
                <Button variant="outline" size="sm" disabled={update.isPending} onClick={() => update.mutate({ id: selected.id, data: { refresh_enabled: !selected.refresh_enabled } })}>
                  {selected.refresh_enabled ? "Pause schedule" : "Resume schedule"}
                </Button>
                <Button size="sm" onClick={runRefresh} disabled={refresh.isPending}>
                  <RefreshCw className={`mr-2 size-4 ${refresh.isPending ? "animate-spin" : ""}`} /> Refresh now
                </Button>
              </div>
            </div>

            <div className="grid gap-4 xl:grid-cols-[2fr_1fr]">
              <Card>
                <CardHeader><CardTitle className="flex items-center gap-2 text-sm"><Users className="size-4" /> Current members</CardTitle></CardHeader>
                <CardContent className="space-y-1">
                  {members.isLoading && <Skeleton className="h-28 w-full" />}
                  {members.data?.map((member) => (
                    <a key={member.lead_id} href={`/leads/${member.lead_id}`} className="flex items-center justify-between rounded-md px-2 py-2 text-sm hover:bg-muted/60">
                      <span className="truncate font-medium">{member.snapshot.company || `Lead ${member.lead_id}`}</span>
                      <span className="ml-3 text-xs text-muted-foreground">{member.snapshot.score ?? "—"} score</span>
                    </a>
                  ))}
                  {!members.isLoading && !members.data?.length && <p className="py-8 text-center text-xs text-muted-foreground">No leads currently match this audience.</p>}
                </CardContent>
              </Card>

              <Card>
                <CardHeader><CardTitle className="flex items-center gap-2 text-sm"><Activity className="size-4" /> Membership activity</CardTitle></CardHeader>
                <CardContent className="space-y-2">
                  {events.isLoading && <Skeleton className="h-28 w-full" />}
                  {events.data?.map((event) => (
                    <div key={event.id} className="flex gap-2 rounded-md border p-2 text-xs">
                      {event.event_type === "entered" ? <ArrowDownLeft className="size-4 shrink-0 text-emerald-500" /> : <ArrowUpRight className="size-4 shrink-0 text-amber-500" />}
                      <div className="min-w-0"><p className="truncate font-medium">{event.snapshot.company || `Lead ${event.lead_id}`}</p><p className="text-muted-foreground">{event.event_type} · {ago(event.created_at)}</p></div>
                    </div>
                  ))}
                  {!events.isLoading && !events.data?.length && <p className="py-8 text-center text-xs text-muted-foreground">Refresh to establish membership history.</p>}
                </CardContent>
              </Card>
            </div>
          </div>
        )}
      </main>
    </div>
  )
}
