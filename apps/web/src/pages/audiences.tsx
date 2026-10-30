import { DestinationSetup } from "@/features/activation/destination-setup"
import { AudienceControls } from "@/features/activation/audience-controls"
import { DestinationWorkbench } from "@/features/activation/destination-workbench"
import { useActivationAccess } from "@/features/activation/use-activation-access"
import { AudienceMemberItem } from "@/features/activation/audience-member"
import { Link, useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import { getActiveWorkspace } from "@/lib/auth"
import { recordAPI } from "@/lib/records"
import { relativeTime } from "@/lib/record-overview"
import {
  audienceSubject,
  audienceScope
} from "@/features/activation/audience-subject"
import {
  PageHeader,
  ErrorState,
  ViewState,
  StatusBadge
} from "@/design-system/patterns/workbench"
import "./audiences.css"
import { useState } from "react"
import {
  Activity,
  ArrowDownLeft,
  ArrowUpRight,
  Building2,
  Send,
  Users
} from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import {
  useAudienceDestinations,
  useAudienceEvents,
  useAudienceMembers,
  useAudiences,
  useDestinationTypes
} from "@/lib/hooks"

const ago = (value: string | null) => (value ? relativeTime(value) : "Never")

interface AudienceAccount {
  key: string
  company: string
  website: string
  contacts: number
  with_email: number
  with_phone: number
  decision_makers: number
  avg_score: number
  email_coverage_pct: number
  phone_coverage_pct: number
  signal_count: number
  signal_weight: number
  profiles: { lead_id: number; name: string; title: string; email: string }[]
}

export default function AudiencesPage() {
  const audiences = useAudiences()
  const [params, setParams] = useSearchParams()
  const selectedId = params.get("audience") ?? audiences.data?.[0]?.id ?? null
  const setSelectedId = (id: string) =>
    setParams((previous) => {
      const next = new URLSearchParams(previous)
      next.set("audience", id)
      next.delete("members_offset")
      return next
    })
  const rawOffset = Number(params.get("members_offset"))
  const offset =
    Number.isSafeInteger(rawOffset) && rawOffset >= 0 ? rawOffset : 0
  const setOffset = (value: number) =>
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous)
        if (value) next.set("members_offset", String(value))
        else next.delete("members_offset")
        return next
      },
      { replace: true }
    )
  const selected = audiences.data?.find(
    (audience) => audience.id === selectedId
  )
  const members = useAudienceMembers(selectedId, offset)
  const events = useAudienceEvents(selectedId)
  const destinations = useAudienceDestinations(selectedId)
  const destinationTypes = useDestinationTypes()
  const activationAccess = useActivationAccess()
  const canActivate =
    activationAccess.data?.can_write === true && !activationAccess.isError
  const accountQuery = useQuery({
    queryKey: [
      "audience-accounts",
      getActiveWorkspace(),
      selectedId,
      selected?.refreshed_at
    ],
    queryFn: ({ signal }) =>
      recordAPI<{
        accounts: AudienceAccount[]
        summary: {
          account_count: number
          contact_count: number
          accounts_with_signals: number
          accounts_with_decision_makers: number
        }
        pagination: {
          limit: number
          offset: number
          next_offset: number | null
          total: number
        }
      }>(`audiences/${selectedId}/accounts`, undefined, "GET", signal),
    enabled: !!selected && audienceScope(selected.filters) === "Legacy leads"
  })
  const accountRollup = accountQuery.data
  const [refreshReceipt, setRefreshReceipt] = useState<{
    id: string
    entered: number
    exited: number
    changed: number
    unchanged: number
    refreshed_at: string | null
  } | null>(null)

  return (
    <div className="workbench-page audiences-page">
      <PageHeader
        title="Audiences"
        description="Review record segments and their activation history."
        actions={
          <Button
            variant="outline"
            render={<Link to="/people" />}
            nativeButton={false}
          >
            Create from people
          </Button>
        }
      />
      <div className="audiences-layout">
        <aside className="audiences-list" aria-label="Saved audiences">
          <label className="gtm-field-label" htmlFor="audience-picker">
            Saved audiences
          </label>
          <select
            id="audience-picker"
            className="audiences-picker"
            value={selectedId ?? ""}
            onChange={(event) => setSelectedId(event.target.value)}
          >
            <option value="" disabled>
              Choose an audience
            </option>
            {audiences.data?.map((audience) => (
              <option value={audience.id} key={audience.id}>
                {audience.name}
              </option>
            ))}
          </select>
          {audiences.isError && audiences.data && (
            <ErrorState
              error={audiences.error}
              retry={() => void audiences.refetch()}
            />
          )}
          {audiences.isLoading && <Skeleton className="h-24 w-full" />}
          {audiences.data?.map((audience) => (
            <button
              key={audience.id}
              onClick={() => setSelectedId(audience.id)}
              aria-current={selectedId === audience.id ? "true" : undefined}
              className={`audiences-list-item w-full rounded-lg border p-3 text-left transition-colors ${selectedId === audience.id ? "border-primary bg-primary/5" : "hover:bg-muted/50"}`}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm font-medium">
                  {audience.name}
                </span>
                <Badge variant="secondary">
                  {audience.refreshed_at
                    ? audience.member_count
                    : "Not refreshed"}
                </Badge>
              </div>
              <p className="mt-2 text-xs text-muted-foreground">
                {audienceScope(audience.filters)} · Refreshed{" "}
                {ago(audience.refreshed_at)}
              </p>
            </button>
          ))}
          {!audiences.isLoading &&
            !audiences.isError &&
            !audiences.data?.length && (
              <Card>
                <CardContent className="p-3 text-xs text-muted-foreground">
                  Select companies or people to create your first audience.
                </CardContent>
              </Card>
            )}
        </aside>

        <section aria-label="Audience details" className="audiences-detail">
          {!selected ? (
            <ViewState
              title={
                audiences.isPending
                  ? "Loading audiences…"
                  : selectedId
                    ? "Audience unavailable"
                    : "Choose an audience"
              }
              kind={
                audiences.isPending ? "loading" : selectedId ? "error" : "empty"
              }
              description={
                selectedId
                  ? "Choose another saved audience or retry the list."
                  : "Create a segment from Companies or People, then review its members here."
              }
              action={
                <Button
                  variant="outline"
                  render={<Link to="/companies" />}
                  nativeButton={false}
                >
                  Browse companies
                </Button>
              }
            />
          ) : (
            <div className="space-y-3">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                  <div className="flex items-center gap-2">
                    <h2 className="text-lg font-semibold">{selected.name}</h2>
                    <StatusBadge
                      tone={
                        selected.refresh_health === "healthy"
                          ? "positive"
                          : selected.refresh_health === "degraded"
                            ? "warning"
                            : "neutral"
                      }
                    >
                      {selected.refresh_health === "healthy"
                        ? "Refresh healthy"
                        : selected.refresh_health === "degraded"
                          ? "Needs review"
                          : "Not refreshed"}
                    </StatusBadge>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {audienceScope(selected.filters)} ·{" "}
                    {selected.refreshed_at
                      ? `${selected.member_count} members in the latest snapshot`
                      : "No membership snapshot yet"}
                  </p>
                  {selected.last_refresh_error && (
                    <p
                      className="mt-1 text-xs text-destructive"
                      title={selected.last_refresh_error}
                    >
                      Refresh failed {selected.consecutive_refresh_failures}×:{" "}
                      {selected.last_refresh_error}
                    </p>
                  )}
                </div>
                <AudienceControls
                  key={selected.id}
                  audience={selected}
                  onRefreshed={(result) =>
                    setRefreshReceipt({
                      id: result.audience.id,
                      entered: result.entered,
                      exited: result.exited,
                      changed: result.changed,
                      unchanged: result.unchanged,
                      refreshed_at: result.audience.refreshed_at
                    })
                  }
                />
              </div>

              {refreshReceipt?.id === selected.id && (
                <div role="status" className="audiences-receipt">
                  <strong>Membership refreshed</strong>
                  <span>
                    Last confirmed refresh ·{" "}
                    {relativeTime(refreshReceipt.refreshed_at)}
                  </span>
                  <span>
                    {refreshReceipt.entered} entered · {refreshReceipt.exited}{" "}
                    exited · {refreshReceipt.changed} changed ·{" "}
                    {refreshReceipt.unchanged} unchanged
                  </span>
                </div>
              )}
              {accountQuery.isError && (
                <ErrorState
                  error={accountQuery.error}
                  retry={() => void accountQuery.refetch()}
                />
              )}
              <div className="grid gap-3 xl:grid-cols-[2fr_1fr]">
                {accountRollup && (
                  <Card className="xl:col-span-2">
                    <CardHeader>
                      <div className="flex items-center justify-between">
                        <CardTitle className="flex items-center gap-2 text-sm">
                          <Building2 className="size-4" /> Account rollup
                        </CardTitle>
                        <div className="flex flex-wrap gap-2">
                          <Badge variant="outline">
                            {accountRollup.summary.account_count} accounts
                          </Badge>
                          <Badge variant="outline">
                            {accountRollup.summary.accounts_with_signals}{" "}
                            signaling
                          </Badge>
                          <Badge variant="outline">
                            {
                              accountRollup.summary
                                .accounts_with_decision_makers
                            }{" "}
                            with decision makers
                          </Badge>
                        </div>
                      </div>
                    </CardHeader>
                    <CardContent className="grid gap-2 md:grid-cols-2 xl:grid-cols-3">
                      {accountRollup.accounts.slice(0, 12).map((account) => (
                        <div
                          key={account.key}
                          className="rounded-md border p-3"
                        >
                          <div className="flex items-start justify-between gap-2">
                            <div className="min-w-0">
                              <p className="truncate text-sm font-medium">
                                {account.company}
                              </p>
                              <p className="truncate text-xs text-muted-foreground">
                                {account.key}
                              </p>
                            </div>
                            <Badge
                              variant={
                                account.signal_weight > 0
                                  ? "default"
                                  : "secondary"
                              }
                            >
                              {account.signal_weight} intent
                            </Badge>
                          </div>
                          <div className="mt-3 grid grid-cols-3 gap-2 text-center">
                            <div>
                              <p className="font-semibold">
                                {account.contacts}
                              </p>
                              <p className="text-xs text-muted-foreground">
                                contacts
                              </p>
                            </div>
                            <div>
                              <p className="font-semibold">
                                {account.decision_makers}
                              </p>
                              <p className="text-xs text-muted-foreground">
                                decision makers
                              </p>
                            </div>
                            <div>
                              <p className="font-semibold">
                                {account.avg_score}
                              </p>
                              <p className="text-xs text-muted-foreground">
                                avg score
                              </p>
                            </div>
                          </div>
                          <div className="mt-3 flex gap-3 text-xs text-muted-foreground">
                            <span>{account.email_coverage_pct}% email</span>
                            <span>{account.phone_coverage_pct}% phone</span>
                            <span>{account.signal_count} signals</span>
                          </div>
                          <div className="mt-2 flex -space-x-1">
                            {account.profiles.slice(0, 4).map((profile) => (
                              <a
                                key={profile.lead_id}
                                href={`/leads/${profile.lead_id}`}
                                title={`${profile.name}${profile.title ? ` · ${profile.title}` : ""}`}
                                className="grid size-7 place-items-center rounded-full border bg-background text-xs font-medium hover:z-10 hover:border-primary"
                              >
                                {profile.name
                                  .split(/\s+/)
                                  .map((part) => part[0])
                                  .join("")
                                  .slice(0, 2)
                                  .toUpperCase()}
                              </a>
                            ))}
                          </div>
                        </div>
                      ))}
                      {!accountRollup.accounts.length && (
                        <p className="py-3 text-xs text-muted-foreground">
                          No accounts in this audience yet.
                        </p>
                      )}
                    </CardContent>
                  </Card>
                )}
                <Card>
                  <CardHeader>
                    <CardTitle className="flex items-center gap-2 text-sm">
                      <Users className="size-4" /> Current members
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="space-y-1">
                    {members.isLoading && <Skeleton className="h-28 w-full" />}
                    {members.isError && (
                      <ErrorState
                        error={members.error}
                        retry={() => void members.refetch()}
                      />
                    )}
                    {members.data?.map((member) => (
                      <AudienceMemberItem member={member} key={member.id} />
                    ))}
                    {!members.isLoading &&
                      !members.isError &&
                      !members.data?.length && (
                        <p className="py-8 text-center text-xs text-muted-foreground">
                          No records on this page. Refresh membership or return
                          to the first page.
                        </p>
                      )}
                    {!!selected.member_count && (
                      <div className="audiences-pagination">
                        <span>
                          {offset + 1}–
                          {Math.min(
                            offset + (members.data?.length ?? 0),
                            selected.member_count
                          )}{" "}
                          of {selected.member_count}
                        </span>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={!offset || members.isFetching}
                          onClick={() => setOffset(Math.max(0, offset - 100))}
                        >
                          Previous
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={
                            offset + 100 >= selected.member_count ||
                            members.isFetching ||
                            !members.data?.length
                          }
                          onClick={() => setOffset(offset + 100)}
                        >
                          Next
                        </Button>
                      </div>
                    )}
                  </CardContent>
                </Card>

                <Card>
                  <CardHeader>
                    <CardTitle className="flex items-center gap-2 text-sm">
                      <Activity className="size-4" /> Membership activity
                    </CardTitle>
                  </CardHeader>
                  <CardContent className="space-y-2">
                    {events.isLoading && <Skeleton className="h-28 w-full" />}
                    {events.isError && (
                      <ErrorState
                        error={events.error}
                        retry={() => void events.refetch()}
                      />
                    )}
                    {events.data?.map((event) => (
                      <div
                        key={event.id}
                        className="flex gap-2 rounded-md border p-2 text-xs"
                      >
                        {event.event_type === "entered" ? (
                          <ArrowDownLeft className="size-4 shrink-0 text-[var(--gtm-success)]" />
                        ) : (
                          <ArrowUpRight className="size-4 shrink-0 text-[var(--gtm-warning)]" />
                        )}
                        <div className="min-w-0">
                          <p className="truncate font-medium">
                            {audienceSubject(event).name}
                          </p>
                          <p className="text-muted-foreground">
                            {event.event_type} · {ago(event.created_at)}
                          </p>
                        </div>
                      </div>
                    ))}
                    {!events.isLoading &&
                      !events.isError &&
                      !events.data?.length && (
                        <p className="py-8 text-center text-xs text-muted-foreground">
                          Refresh to establish membership history.
                        </p>
                      )}
                  </CardContent>
                </Card>
              </div>

              <Card>
                <CardHeader>
                  <CardTitle className="flex items-center gap-2 text-sm">
                    <Send className="size-4" /> Activation destinations
                  </CardTitle>
                </CardHeader>
                <CardContent className="space-y-3">
                  {destinations.isError && (
                    <ErrorState
                      error={destinations.error}
                      retry={() => void destinations.refetch()}
                    />
                  )}
                  {destinationTypes.isError && (
                    <div role="alert" className="destination-error">
                      <p>Integration status could not be checked.</p>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => void destinationTypes.refetch()}
                      >
                        Try integration status again
                      </Button>
                    </div>
                  )}
                  {activationAccess.isPending && (
                    <p role="status" className="text-xs text-muted-foreground">
                      Checking activation permissions…
                    </p>
                  )}
                  {activationAccess.isError && (
                    <div role="alert" className="text-xs text-destructive">
                      Activation permissions could not be checked.{" "}
                      <button
                        type="button"
                        className="underline"
                        onClick={() => void activationAccess.refetch()}
                      >
                        Try permissions again
                      </button>
                    </div>
                  )}
                  {!activationAccess.isPending &&
                    !activationAccess.isError &&
                    !canActivate && (
                      <p
                        role="status"
                        className="text-xs text-muted-foreground"
                      >
                        View only. Activation write permission is required to
                        change destinations or send records.
                      </p>
                    )}
                  <DestinationSetup
                    key={`${selected.id}:${params.toString()}`}
                    audience={selected}
                  />
                  {destinations.data?.map((destination) => (
                    <DestinationWorkbench
                      key={destination.id}
                      destination={destination}
                      audience={selected}
                      maturity={
                        destinationTypes.data?.find(
                          (type) => type.id === destination.destination_type
                        )?.maturity ??
                        (destinationTypes.isPending
                          ? "Checking status"
                          : "Status unavailable")
                      }
                    />
                  ))}
                  {!destinations.isLoading &&
                    !destinations.isError &&
                    !destinations.data?.length && (
                      <p className="py-3 text-center text-xs text-muted-foreground">
                        Connect a destination to activate this audience.
                      </p>
                    )}
                </CardContent>
              </Card>
            </div>
          )}
        </section>
      </div>
    </div>
  )
}
