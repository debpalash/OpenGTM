import { useEffect, useState } from "react"
import { Activity, ArrowDownLeft, ArrowUpRight, ListFilter, Plus, RefreshCw, Send, Users } from "lucide-react"
import { toast } from "sonner"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { useAudienceDestinations, useAudienceEvents, useAudienceMembers, useAudiences, useCreateAudienceDestination, useRefreshAudience, useSyncAudienceDestination, useUpdateAudience } from "@/lib/hooks"
import type { AudienceDestination } from "@/lib/api"

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
  const destinations = useAudienceDestinations(selectedId)
  const createDestination = useCreateAudienceDestination()
  const syncDestination = useSyncAudienceDestination(selectedId)
  const [destinationType, setDestinationType] = useState<AudienceDestination["destination_type"]>("webhook")
  const [destinationName, setDestinationName] = useState("")
  const [webhookUrl, setWebhookUrl] = useState("")
  const [platformListId, setPlatformListId] = useState("")
  const [googleCustomerId, setGoogleCustomerId] = useState("")
  const [consentSource, setConsentSource] = useState("")

  const runRefresh = async () => {
    if (!selectedId) return
    try {
      const result = await refresh.mutateAsync(selectedId)
      toast.success(`Audience refreshed: +${result.entered} entered, −${result.exited} exited, ${result.changed} changed`)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Could not refresh audience")
    }
  }

  const addDestination = async () => {
    if (!selectedId || !destinationName.trim()) return
    try {
      const adConfig = destinationType === "meta_ads" ? { custom_audience_id: platformListId.trim() }
        : destinationType === "linkedin_ads" ? { segment_id: platformListId.trim() }
        : destinationType === "google_ads" ? { customer_id: googleCustomerId.trim(), user_list_id: platformListId.trim() } : {}
      await createDestination.mutateAsync({
        audience_id: selectedId, name: destinationName.trim(), destination_type: destinationType,
        config: destinationType === "webhook" ? { url: webhookUrl.trim(), method: "POST" }
          : destinationType.endsWith("_ads") ? { ...adConfig, consent_attested: true, consent_source: consentSource.trim() } : {},
      })
      setDestinationName(""); setWebhookUrl(""); setPlatformListId(""); setGoogleCustomerId(""); setConsentSource("")
      toast.success("Destination added")
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Could not add destination")
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

            <Card>
              <CardHeader><CardTitle className="flex items-center gap-2 text-sm"><Send className="size-4" /> Activation destinations</CardTitle></CardHeader>
              <CardContent className="space-y-3">
                <div className="flex flex-wrap items-center gap-2 rounded-md border p-2">
                  <select value={destinationType} onChange={(event) => setDestinationType(event.target.value as AudienceDestination["destination_type"])} className="h-8 rounded-md border bg-background px-2 text-xs">
                    <option value="webhook">Webhook</option><option value="hubspot">HubSpot</option><option value="salesforce">Salesforce</option><option value="meta_ads">Meta Ads</option><option value="google_ads">Google Ads</option><option value="linkedin_ads">LinkedIn Ads</option>
                  </select>
                  <Input value={destinationName} onChange={(event) => setDestinationName(event.target.value)} placeholder="Destination name" className="h-8 min-w-40 flex-1" />
                  {destinationType === "webhook" && <Input value={webhookUrl} onChange={(event) => setWebhookUrl(event.target.value)} placeholder="https://…" className="h-8 min-w-64 flex-[2]" />}
                  {destinationType.endsWith("_ads") && <><Input value={platformListId} onChange={(event) => setPlatformListId(event.target.value)} placeholder={destinationType === "meta_ads" ? "Custom audience ID" : destinationType === "linkedin_ads" ? "Segment ID" : "User list ID"} className="h-8 min-w-40" />{destinationType === "google_ads" && <Input value={googleCustomerId} onChange={(event) => setGoogleCustomerId(event.target.value)} placeholder="Customer ID" className="h-8 min-w-36" />}<Input value={consentSource} onChange={(event) => setConsentSource(event.target.value)} placeholder="Consent source / policy" className="h-8 min-w-48" /></>}
                  <Button size="sm" onClick={addDestination} disabled={createDestination.isPending || !destinationName.trim() || (destinationType === "webhook" && !webhookUrl.trim()) || (destinationType.endsWith("_ads") && (!platformListId.trim() || !consentSource.trim() || (destinationType === "google_ads" && !googleCustomerId.trim())))}><Plus className="mr-1 size-3" /> Add</Button>
                </div>
                {destinationType.endsWith("_ads") && <p className="text-xs text-muted-foreground">Adding this destination attests that the audience has valid advertising consent. OpenGTM SHA-256 hashes identifiers before upload; platform API approval may be required.</p>}
                {destinations.data?.map((destination) => (
                  <div key={destination.id} className="flex flex-wrap items-center justify-between gap-3 rounded-md border p-3">
                    <div><div className="flex items-center gap-2"><span className="text-sm font-medium">{destination.name}</span><Badge variant="outline">{destination.destination_type}</Badge><Badge variant={destination.health_status === "healthy" ? "default" : "secondary"}>{destination.health_status}</Badge></div><p className="mt-1 text-xs text-muted-foreground">{destination.last_error || (destination.last_success_at ? `Last synced ${ago(destination.last_success_at)}` : "Not synced yet")}</p></div>
                    <Button variant="outline" size="sm" disabled={syncDestination.isPending} onClick={() => syncDestination.mutate(destination.id, { onSuccess: (run) => toast.success(`Sync queued: ${run.id.slice(0, 8)}`), onError: (error) => toast.error(error.message) })}><Send className="mr-1 size-3" /> Sync audience</Button>
                  </div>
                ))}
                {!destinations.isLoading && !destinations.data?.length && <p className="py-4 text-center text-xs text-muted-foreground">Connect a destination to activate this audience.</p>}
              </CardContent>
            </Card>
          </div>
        )}
      </main>
    </div>
  )
}
