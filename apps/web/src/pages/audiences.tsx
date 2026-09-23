import { useEffect, useState } from "react"
import { Activity, ArrowDownLeft, ArrowUpRight, Building2, Copy, KeyRound, ListFilter, Plus, RefreshCw, Send, StopCircle, Users } from "lucide-react"
import { toast } from "sonner"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { useAudienceDestinations, useAudienceEvents, useAudienceMembers, useAudiences, useCancelAudienceDestination, useCreateAudienceDestination, useDestinationTypes, useRefreshAudience, useRetryAudienceDestination, useSyncAudienceDestination, useUpdateAudience } from "@/lib/hooks"
import type { AudienceDestination } from "@/lib/api"
import { NativeSelect } from "@/components/ui/native-select"

const ago = (value: string | null) => {
  if (!value) return "Never"
  const minutes = Math.max(0, Math.round((Date.now() - new Date(value).getTime()) / 60_000))
  if (minutes < 1) return "Just now"
  if (minutes < 60) return `${minutes}m ago`
  if (minutes < 1440) return `${Math.round(minutes / 60)}h ago`
  return `${Math.round(minutes / 1440)}d ago`
}

interface AudienceAccount {
  key: string; company: string; website: string; contacts: number; with_email: number; with_phone: number
  decision_makers: number; avg_score: number; email_coverage_pct: number; phone_coverage_pct: number
  signal_count: number; signal_weight: number; profiles: { lead_id: number; name: string; title: string; email: string }[]
}
interface InboundReceipt { id: string; destination_id: string; provider: string; external_event_id: string; lead_id: number | null; status: string; conflict_policy: string; applied_fields: string[]; ignored_fields: string[]; error: string | null; created_at: string }

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
  const destinationTypes = useDestinationTypes()
  const createDestination = useCreateAudienceDestination()
  const syncDestination = useSyncAudienceDestination(selectedId)
  const retryDestination = useRetryAudienceDestination(selectedId)
  const cancelDestination = useCancelAudienceDestination(selectedId)
  const [destinationType, setDestinationType] = useState<AudienceDestination["destination_type"]>("webhook")
  const [destinationName, setDestinationName] = useState("")
  const [webhookUrl, setWebhookUrl] = useState("")
  const [platformListId, setPlatformListId] = useState("")
  const [googleCustomerId, setGoogleCustomerId] = useState("")
  const [consentSource, setConsentSource] = useState("")
  const [warehouseUrl, setWarehouseUrl] = useState("")
  const [warehouseSecretRef, setWarehouseSecretRef] = useState("")
  const [warehouseDataset, setWarehouseDataset] = useState("opengtm_audience")
  const [campaignId, setCampaignId] = useState("")
  const [spreadsheetId, setSpreadsheetId] = useState("")
  const [sheetRange, setSheetRange] = useState("Sheet1!A:ZZ")
  const [sheetColumns, setSheetColumns] = useState("company,email,phone,website")
  const [airtableBase, setAirtableBase] = useState("")
  const [airtableTable, setAirtableTable] = useState("")
  const [airtableKeyField, setAirtableKeyField] = useState("OpenGTM ID")
  const [slackSecretRef, setSlackSecretRef] = useState("")
  const [slackTemplate, setSlackTemplate] = useState("New audience member: {company} {contact_person} {email}")
  const [inboundPolicy, setInboundPolicy] = useState("fill_missing")
  const [inboundSetup, setInboundSetup] = useState<{ destinationId: string; token?: string; receipts: InboundReceipt[]; nextCursor: string | null } | null>(null)
  const [loadingInboundMore, setLoadingInboundMore] = useState(false)
  const selectedDestinationType = destinationTypes.data?.find((item) => item.id === destinationType)
  const [accountRollup, setAccountRollup] = useState<{ accounts: AudienceAccount[]; summary: { account_count: number; contact_count: number; accounts_with_signals: number; accounts_with_decision_makers: number }; pagination: { limit: number; offset: number; next_offset: number | null; total: number } } | null>(null)
  useEffect(() => {
    if (!selectedId) { setAccountRollup(null); return }
    fetch(`/api/audiences/${selectedId}/accounts`).then(response => response.ok ? response.json() : Promise.reject()).then(setAccountRollup).catch(() => setAccountRollup(null))
  }, [selectedId, selected?.refreshed_at])

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
          : destinationType.endsWith("_ads") ? { ...adConfig, consent_attested: true, consent_source: consentSource.trim() }
          : destinationType === "hubspot" || destinationType === "salesforce" ? { inbound_conflict_policy: inboundPolicy }
          : destinationType === "instantly" ? { campaign_id: campaignId.trim(), skip_if_in_campaign: true }
          : destinationType === "smartlead" ? { campaign_id: campaignId.trim() }
          : destinationType === "google_sheets" ? { spreadsheet_id: spreadsheetId.trim(), range: sheetRange.trim(), columns: sheetColumns.split(",").map(value => value.trim()).filter(Boolean) }
          : destinationType === "airtable" ? { base_id: airtableBase.trim(), table: airtableTable.trim(), idempotency_field: airtableKeyField.trim(), typecast: true }
          : destinationType === "slack" ? { webhook_secret_ref: slackSecretRef.trim(), message_template: slackTemplate.trim() }
          : destinationType === "warehouse_http" ? { url: warehouseUrl.trim(), header_secret_ref: warehouseSecretRef.trim(), dataset: warehouseDataset.trim(), mode: "snapshot" } : {},
      })
      setDestinationName(""); setWebhookUrl(""); setPlatformListId(""); setGoogleCustomerId(""); setConsentSource(""); setWarehouseUrl(""); setWarehouseSecretRef(""); setCampaignId(""); setSpreadsheetId(""); setAirtableBase(""); setAirtableTable(""); setSlackSecretRef("")
      toast.success("Destination added")
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Could not add destination")
    }
  }

  const showInbound = async (destination: AudienceDestination, rotate = false) => {
    try {
      let token: string | undefined
      if (rotate) {
        const response = await fetch(`/api/audience-destinations/${destination.id}/inbound-token`, { method: "POST" })
        if (!response.ok) throw new Error(`Could not create inbound token (${response.status})`)
        token = (await response.json()).token
      }
      const response = await fetch(`/api/audience-destinations/${destination.id}/inbound-receipts`)
      if (!response.ok) throw new Error(`Could not load inbound history (${response.status})`)
      const page = await response.json()
      setInboundSetup({ destinationId: destination.id, token, receipts: page.receipts || [], nextCursor: page.next_cursor || null })
      if (token) toast.success("Inbound token rotated; copy it now")
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not configure inbound sync") }
  }

  const loadMoreInbound = async () => {
    if (!inboundSetup?.nextCursor) return
    setLoadingInboundMore(true)
    try {
      const query = new URLSearchParams({ cursor: inboundSetup.nextCursor })
      const response = await fetch(`/api/audience-destinations/${inboundSetup.destinationId}/inbound-receipts?${query}`)
      if (!response.ok) throw new Error(`Could not load inbound history (${response.status})`)
      const page = await response.json()
      setInboundSetup(current => current && current.destinationId === inboundSetup.destinationId ? {
        ...current,
        receipts: [...current.receipts, ...(page.receipts || []).filter((receipt: InboundReceipt) => !current.receipts.some(item => item.id === receipt.id))],
        nextCursor: page.next_cursor || null,
      } : current)
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not load inbound history") }
    setLoadingInboundMore(false)
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
                <div className="flex items-center gap-2"><h2 className="text-lg font-semibold">{selected.name}</h2><Badge variant={selected.refresh_health === "healthy" ? "default" : "secondary"}>{selected.refresh_health}</Badge></div>
                <p className="text-xs text-muted-foreground">{Object.keys(selected.filters).length} active filters · {selected.member_count} current members</p>
                {selected.last_refresh_error && <p className="mt-1 text-xs text-destructive" title={selected.last_refresh_error}>Refresh failed {selected.consecutive_refresh_failures}×: {selected.last_refresh_error}</p>}
              </div>
              <div className="flex items-center gap-2">
                <NativeSelect
                  aria-label="Audience refresh interval"
                  value={selected.refresh_interval_minutes}
                  disabled={!selected.refresh_enabled || update.isPending}
                  onChange={(event) => update.mutate({ id: selected.id, data: { refresh_interval_minutes: Number(event.target.value) } })}
                >
                  <option value={15}>Every 15 minutes</option>
                  <option value={60}>Hourly</option>
                  <option value={360}>Every 6 hours</option>
                  <option value={1440}>Daily</option>
                  <option value={10080}>Weekly</option>
                </NativeSelect>
                <Button variant="outline" size="sm" disabled={update.isPending} onClick={() => update.mutate({ id: selected.id, data: { refresh_enabled: !selected.refresh_enabled } })}>
                  {selected.refresh_enabled ? "Pause schedule" : "Resume schedule"}
                </Button>
                <Button size="sm" onClick={runRefresh} disabled={refresh.isPending}>
                  <RefreshCw className={`mr-2 size-4 ${refresh.isPending ? "animate-spin" : ""}`} /> Refresh now
                </Button>
              </div>
            </div>

            <div className="grid gap-4 xl:grid-cols-[2fr_1fr]">
              {accountRollup && <Card className="xl:col-span-2"><CardHeader><div className="flex items-center justify-between"><CardTitle className="flex items-center gap-2 text-sm"><Building2 className="size-4" /> Account rollup</CardTitle><div className="flex gap-2"><Badge variant="outline">{accountRollup.summary.account_count} accounts</Badge><Badge variant="outline">{accountRollup.summary.accounts_with_signals} signaling</Badge><Badge variant="outline">{accountRollup.summary.accounts_with_decision_makers} with decision makers</Badge></div></div></CardHeader><CardContent className="grid gap-2 md:grid-cols-2 xl:grid-cols-3">{accountRollup.accounts.slice(0, 12).map(account => <div key={account.key} className="rounded-md border p-3"><div className="flex items-start justify-between gap-2"><div className="min-w-0"><p className="truncate text-sm font-medium">{account.company}</p><p className="truncate text-[10px] text-muted-foreground">{account.key}</p></div><Badge variant={account.signal_weight > 0 ? "default" : "secondary"}>{account.signal_weight} intent</Badge></div><div className="mt-3 grid grid-cols-3 gap-2 text-center"><div><p className="font-semibold">{account.contacts}</p><p className="text-[9px] text-muted-foreground">contacts</p></div><div><p className="font-semibold">{account.decision_makers}</p><p className="text-[9px] text-muted-foreground">decision makers</p></div><div><p className="font-semibold">{account.avg_score}</p><p className="text-[9px] text-muted-foreground">avg score</p></div></div><div className="mt-3 flex gap-3 text-[10px] text-muted-foreground"><span>{account.email_coverage_pct}% email</span><span>{account.phone_coverage_pct}% phone</span><span>{account.signal_count} signals</span></div><div className="mt-2 flex -space-x-1">{account.profiles.slice(0, 4).map(profile => <a key={profile.lead_id} href={`/leads/${profile.lead_id}`} title={`${profile.name}${profile.title ? ` · ${profile.title}` : ""}`} className="grid size-7 place-items-center rounded-full border bg-background text-[9px] font-medium hover:z-10 hover:border-primary">{profile.name.split(/\s+/).map(part => part[0]).join("").slice(0, 2).toUpperCase()}</a>)}</div></div>)}{!accountRollup.accounts.length && <p className="py-4 text-xs text-muted-foreground">No accounts in this audience yet.</p>}</CardContent></Card>}
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
                  <NativeSelect aria-label="Destination type" value={destinationType} onChange={(event) => setDestinationType(event.target.value as AudienceDestination["destination_type"])}>
                    <option value="webhook">Webhook</option><option value="slack">Slack</option><option value="hubspot">HubSpot</option><option value="salesforce">Salesforce</option><option value="warehouse_http">Warehouse HTTP</option><option value="google_sheets">Google Sheets</option><option value="airtable">Airtable</option><option value="instantly">Instantly</option><option value="smartlead">Smartlead</option><option value="meta_ads">Meta Ads</option><option value="google_ads">Google Ads</option><option value="linkedin_ads">LinkedIn Ads</option>
                  </NativeSelect>
                  <Badge variant={selectedDestinationType?.maturity === "supported" ? "default" : "secondary"}>{selectedDestinationType?.maturity ?? "beta"}</Badge>
                  <Input value={destinationName} onChange={(event) => setDestinationName(event.target.value)} placeholder="Destination name" className="h-8 min-w-40 flex-1" />
                  {(destinationType === "hubspot" || destinationType === "salesforce") && <NativeSelect aria-label="Inbound sync policy" value={inboundPolicy} onChange={event => setInboundPolicy(event.target.value)}><option value="fill_missing">Inbound: fill missing fields</option><option value="crm_wins">Inbound: CRM wins</option></NativeSelect>}
                  {destinationType === "warehouse_http" && <><Input value={warehouseUrl} onChange={event => setWarehouseUrl(event.target.value)} placeholder="HTTPS ingestion endpoint" className="h-8 min-w-64 flex-[2]" /><Input value={warehouseSecretRef} onChange={event => setWarehouseSecretRef(event.target.value)} placeholder="Workspace secret key" className="h-8 min-w-40" /><Input value={warehouseDataset} onChange={event => setWarehouseDataset(event.target.value)} placeholder="Dataset" className="h-8 min-w-36" /></>}
                  {destinationType === "webhook" && <Input value={webhookUrl} onChange={(event) => setWebhookUrl(event.target.value)} placeholder="https://…" className="h-8 min-w-64 flex-[2]" />}
                  {(destinationType === "instantly" || destinationType === "smartlead") && <Input value={campaignId} onChange={(event) => setCampaignId(event.target.value)} placeholder={`${destinationType === "instantly" ? "Instantly" : "Smartlead"} campaign ID`} className="h-8 min-w-52" />}
                  {destinationType === "google_sheets" && <><Input value={spreadsheetId} onChange={(event) => setSpreadsheetId(event.target.value)} placeholder="Spreadsheet ID" className="h-8 min-w-52" /><Input value={sheetRange} onChange={(event) => setSheetRange(event.target.value)} placeholder="Sheet1!A:ZZ" className="h-8 min-w-40" /><Input value={sheetColumns} onChange={(event) => setSheetColumns(event.target.value)} placeholder="company,email,phone" className="h-8 min-w-56" /></>}
                  {destinationType === "airtable" && <><Input value={airtableBase} onChange={(event) => setAirtableBase(event.target.value)} placeholder="Base ID" className="h-8 min-w-40" /><Input value={airtableTable} onChange={(event) => setAirtableTable(event.target.value)} placeholder="Table name or ID" className="h-8 min-w-44" /><Input value={airtableKeyField} onChange={(event) => setAirtableKeyField(event.target.value)} placeholder="OpenGTM ID field" className="h-8 min-w-44" /></>}
                  {destinationType === "slack" && <><Input value={slackSecretRef} onChange={(event) => setSlackSecretRef(event.target.value)} placeholder="Workspace secret containing webhook URL" className="h-8 min-w-64" /><Input value={slackTemplate} onChange={(event) => setSlackTemplate(event.target.value)} placeholder="Message template" className="h-8 min-w-72 flex-[2]" /></>}
                  {destinationType.endsWith("_ads") && <><Input value={platformListId} onChange={(event) => setPlatformListId(event.target.value)} placeholder={destinationType === "meta_ads" ? "Custom audience ID" : destinationType === "linkedin_ads" ? "Segment ID" : "User list ID"} className="h-8 min-w-40" />{destinationType === "google_ads" && <Input value={googleCustomerId} onChange={(event) => setGoogleCustomerId(event.target.value)} placeholder="Customer ID" className="h-8 min-w-36" />}<Input value={consentSource} onChange={(event) => setConsentSource(event.target.value)} placeholder="Consent source / policy" className="h-8 min-w-48" /></>}
                  <Button size="sm" onClick={addDestination} disabled={createDestination.isPending || !destinationName.trim() || (destinationType === "webhook" && !webhookUrl.trim()) || (destinationType === "slack" && !slackSecretRef.trim()) || (destinationType === "warehouse_http" && (!warehouseUrl.trim() || !warehouseSecretRef.trim() || !warehouseDataset.trim())) || (destinationType === "google_sheets" && (!spreadsheetId.trim() || !sheetRange.trim() || !sheetColumns.trim())) || (destinationType === "airtable" && (!airtableBase.trim() || !airtableTable.trim() || !airtableKeyField.trim())) || ((destinationType === "instantly" || destinationType === "smartlead") && !campaignId.trim()) || (destinationType.endsWith("_ads") && (!platformListId.trim() || !consentSource.trim() || (destinationType === "google_ads" && !googleCustomerId.trim())))}><Plus className="mr-1 size-3" /> Add</Button>
                </div>
                {destinationType.endsWith("_ads") && <p className="text-xs text-muted-foreground">Adding this destination attests that the audience has valid advertising consent. OpenGTM SHA-256 hashes identifiers before upload; platform API approval may be required.</p>}
                {selectedDestinationType?.maturity !== "supported" && <p className="text-xs text-amber-700 dark:text-amber-300">Beta integration: this deployment has no current controlled-live certification. Test with a limited audience before production use.</p>}
                {destinationType === "warehouse_http" && <p className="text-xs text-muted-foreground">Exports one checksum-manifested JSONL snapshot per run. Configure the referenced encrypted secret under Settings → Integrations first.</p>}
                {destinationType === "google_sheets" && <p className="text-xs text-muted-foreground">The first sheet column is reserved for OpenGTM’s stable delivery key, allowing retries to update rather than duplicate rows.</p>}
                {destinationType === "airtable" && <p className="text-xs text-muted-foreground">Create the merge field in Airtable first. OpenGTM uses Airtable’s atomic upsert API so retries update one record.</p>}
                {destinationType === "slack" && <p className="text-xs text-muted-foreground">Store the incoming-webhook URL under Settings → Secrets. Only its secret reference is saved here.</p>}
                {destinations.data?.map((destination) => (
                  <div key={destination.id} className="flex flex-wrap items-center justify-between gap-3 rounded-md border p-3">
                    <div><div className="flex items-center gap-2"><span className="text-sm font-medium">{destination.name}</span><Badge variant="outline">{destination.destination_type}</Badge><Badge variant={destinationTypes.data?.find(item => item.id === destination.destination_type)?.maturity === "supported" ? "default" : "secondary"}>{destinationTypes.data?.find(item => item.id === destination.destination_type)?.maturity ?? "beta"}</Badge><Badge variant={destination.health_status === "healthy" ? "default" : "secondary"}>{destination.health_status}</Badge></div><p className="mt-1 text-xs text-muted-foreground">{destination.last_error || (destination.last_success_at ? `Last synced ${ago(destination.last_success_at)}` : "Not synced yet")}</p></div>
                    <div className="flex gap-2">{(destination.destination_type === "hubspot" || destination.destination_type === "salesforce") && <><Button variant="outline" size="sm" onClick={() => showInbound(destination)}><Activity className="mr-1 size-3" /> Inbound history</Button><Button variant="outline" size="sm" onClick={() => showInbound(destination, true)}><KeyRound className="mr-1 size-3" /> Rotate token</Button></>}{destination.latest_run && ["pending", "running", "cancelling"].includes(destination.latest_run.status) && <Button variant="outline" size="sm" disabled={cancelDestination.isPending || destination.latest_run.status === "cancelling"} onClick={() => cancelDestination.mutate(destination.latest_run!.id, { onSuccess: (run) => toast.success(run.status === "cancelled" ? "Sync cancelled" : "Cancellation requested"), onError: (error) => toast.error(error.message) })}><StopCircle className="mr-1 size-3" /> {destination.latest_run.status === "cancelling" ? "Cancelling…" : "Cancel sync"}</Button>}{destination.latest_run && ["failed", "completed_with_errors"].includes(destination.latest_run.status) && <Button variant="outline" size="sm" disabled={retryDestination.isPending} onClick={() => retryDestination.mutate(destination.latest_run!.id, { onSuccess: () => toast.success("Retrying failed deliveries"), onError: (error) => toast.error(error.message) })}><RefreshCw className="mr-1 size-3" /> Retry failed</Button>}{!destination.latest_run || !["pending", "running", "cancelling"].includes(destination.latest_run.status) ? <Button variant="outline" size="sm" disabled={syncDestination.isPending} onClick={() => syncDestination.mutate(destination.id, { onSuccess: (run) => toast.success(`Sync queued: ${run.id.slice(0, 8)}`), onError: (error) => toast.error(error.message) })}><Send className="mr-1 size-3" /> Sync audience</Button> : null}</div>
                  </div>
                ))}
                {inboundSetup && <div className="space-y-3 rounded-md border border-primary/30 bg-primary/5 p-3"><div><p className="text-xs font-medium">CRM → OpenGTM callback</p><code className="mt-1 block break-all text-[10px] text-muted-foreground">POST {window.location.origin}/api/audience-destinations/inbound/{inboundSetup.destinationId}</code></div>{inboundSetup.token && <div className="rounded-md border border-amber-500/30 bg-amber-500/5 p-2"><p className="text-[10px] font-medium text-amber-700 dark:text-amber-300">Shown once. Store this Bearer token in your CRM webhook secret manager.</p><div className="mt-1 flex items-center gap-2"><code className="min-w-0 flex-1 truncate text-xs">{inboundSetup.token}</code><Button size="sm" variant="outline" className="h-7" onClick={() => { navigator.clipboard.writeText(inboundSetup.token || ""); toast.success("Token copied") }}><Copy className="size-3" /> Copy</Button></div></div>}<p className="text-[10px] text-muted-foreground">Send external_event_id, optional external_record_id or lead_id, and fields. Only allowlisted lead fields are accepted; retries are idempotent.</p><div className="max-h-64 space-y-1 overflow-y-auto">{inboundSetup.receipts.map(receipt => <div key={receipt.id} className="flex items-center justify-between rounded border bg-background px-2 py-1.5 text-[10px]"><span className="truncate">{receipt.external_event_id} · lead {receipt.lead_id ?? "unmatched"} · {receipt.applied_fields.length} fields</span><Badge variant={receipt.status === "unmatched" ? "destructive" : "outline"}>{receipt.status}</Badge></div>)}{inboundSetup.nextCursor && <Button size="sm" variant="outline" className="w-full" disabled={loadingInboundMore} onClick={loadMoreInbound}>{loadingInboundMore && <RefreshCw className="mr-1 size-3 animate-spin" />}Load older callbacks</Button>}{!inboundSetup.receipts.length && <p className="text-[10px] text-muted-foreground">No inbound callbacks received yet.</p>}</div></div>}
                {!destinations.isLoading && !destinations.data?.length && <p className="py-4 text-center text-xs text-muted-foreground">Connect a destination to activate this audience.</p>}
              </CardContent>
            </Card>
          </div>
        )}
      </main>
    </div>
  )
}
