import { useLayoutEffect, useRef, useState } from "react"
import { useQueryClient } from "@tanstack/react-query"
import type { Audience } from "@/lib/api"
import { queryKeys } from "@/lib/query-client"
import { getActiveWorkspace } from "@/lib/auth"
import { Button } from "@/components/ui/button"
import { NativeSelect } from "@/components/ui/native-select"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import { useLeaveGuard } from "@/features/navigation/leave-guard-provider"
import { useActivationAccess } from "./use-activation-access"
import {
  audienceReviewFingerprint,
  destinationTarget,
  fetchActivationAccess,
  fetchAudienceActivationContext,
  performAudienceChange,
  type AudienceActivationContext,
  type AudienceChange,
  type MembershipReceipt
} from "./destination-client"
import "./destination-workbench.css"

/** Refresh changes can queue enabled destinations. Scheduling is therefore part
 * of activation review, rather than an unrelated cadence preference. */
export function AudienceControls({
  audience,
  onRefreshed
}: {
  audience: Audience
  onRefreshed: (receipt: MembershipReceipt) => void
}) {
  const access = useActivationAccess(),
    canWrite = access.data?.can_write === true && !access.isError
  const client = useQueryClient(),
    workspace = getActiveWorkspace()
  const [change, setChange] = useState<AudienceChange | null>(null)
  const [context, setContext] = useState<AudienceActivationContext | null>(null)
  const [phase, setPhase] = useState<
    "ready" | "loading" | "checking" | "sending"
  >("ready")
  const [error, setError] = useState(""),
    [note, setNote] = useState(""),
    [needsReview, setNeedsReview] = useState(false)
  const pending = useRef(false),
    alive = useRef(true),
    controller = useRef<AbortController | null>(null),
    trigger = useRef<HTMLElement | null>(null)
  const sending = phase === "sending"
  useLeaveGuard({
    dirty: false,
    busy: sending,
    busyLabel:
      change?.kind === "refresh"
        ? "Audience refresh in progress"
        : "Schedule update in progress"
  })
  useLayoutEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
      controller.current?.abort()
    }
  }, [])
  async function review(next: AudienceChange) {
    if (pending.current || !canWrite) return
    if (!change) trigger.current = document.activeElement as HTMLElement
    controller.current?.abort()
    const abort = new AbortController()
    controller.current = abort
    setChange(next)
    setContext(null)
    setPhase("loading")
    setError("")
    setNote("")
    setNeedsReview(false)
    try {
      const fresh = await fetchAudienceActivationContext(
        audience.id,
        abort.signal
      )
      if (!abort.signal.aborted && alive.current) setContext(fresh)
    } catch (cause) {
      if (!abort.signal.aborted && alive.current)
        setError(
          cause instanceof Error
            ? cause.message
            : "Audience review could not be loaded."
        )
    } finally {
      if (!abort.signal.aborted && alive.current) setPhase("ready")
    }
  }
  function close() {
    if (sending) return
    controller.current?.abort()
    setChange(null)
    setError("")
  }
  function invalidate() {
    void client.invalidateQueries({ queryKey: queryKeys.audiences.all })
    void client.invalidateQueries({
      queryKey: queryKeys.audienceDestinations(audience.id)
    })
    void client.invalidateQueries({
      queryKey: ["destination-run-history", workspace]
    })
  }
  async function submit(
    next: AudienceChange,
    baseline: AudienceActivationContext | null
  ) {
    if (pending.current || !canWrite) return
    pending.current = true
    setPhase("checking")
    setError("")
    setNote("")
    const abort = new AbortController()
    controller.current = abort
    let submitted = false
    try {
      const [permission, fresh] = await Promise.all([
        fetchActivationAccess(abort.signal),
        baseline
          ? fetchAudienceActivationContext(audience.id, abort.signal)
          : Promise.resolve(null)
      ])
      if (abort.signal.aborted || !alive.current) return
      if (!permission.can_write) {
        void access.refetch()
        throw new Error(
          "Your activation permission changed. This action was not sent."
        )
      }
      if (
        baseline &&
        fresh &&
        audienceReviewFingerprint(baseline) !== audienceReviewFingerprint(fresh)
      )
        throw new Error(
          "Audience or enabled destinations changed after review. Refresh the review before continuing."
        )
      await client.cancelQueries({ queryKey: queryKeys.audiences.all })
      if (abort.signal.aborted || !alive.current) return
      setPhase("sending")
      submitted = true
      const result = await performAudienceChange(audience.id, next)
      if (!alive.current) return
      const confirmed =
        result.kind === "refresh" ? result.receipt.audience : result.audience
      client.setQueryData<Audience[]>(queryKeys.audiences.all, (current) =>
        current?.map((item) => (item.id === audience.id ? confirmed : item))
      )
      if (result.kind === "refresh") onRefreshed(result.receipt)
      setNote(
        result.kind === "refresh"
          ? "Membership refreshed. Changed membership can queue enabled destinations; check their run histories for delivery results."
          : result.audience.refresh_enabled
            ? `Schedule saved: every ${result.audience.refresh_interval_minutes} minutes. Membership changes can queue enabled destinations.`
            : "Schedule paused. Existing sync runs are not cancelled."
      )
      setChange(null)
      invalidate()
    } catch (cause) {
      if (abort.signal.aborted || !alive.current) return
      setError(
        (cause instanceof Error
          ? cause.message
          : "The action could not be confirmed.") +
          (submitted
            ? " The change may have reached the server. Review membership, schedule and destination run history before retrying."
            : "")
      )
      setNeedsReview(true)
      if (submitted) invalidate()
    } finally {
      pending.current = false
      if (alive.current && !abort.signal.aborted) setPhase("ready")
    }
  }
  const enabled = context?.destinations.filter((d) => d.enabled) ?? []
  return (
    <div className="audience-controls">
      <div
        className="flex flex-wrap items-center gap-2 audience-schedule-actions"
        role="group"
        aria-label="Audience refresh and schedule"
      >
        <NativeSelect
          aria-label="Audience refresh interval"
          value={audience.refresh_interval_minutes}
          disabled={!canWrite || !audience.refresh_enabled || phase !== "ready"}
          onChange={(event) =>
            void review({
              kind: "schedule",
              refresh_interval_minutes: Number(event.target.value)
            })
          }
        >
          <option value={15}>Every 15 minutes</option>
          <option value={60}>Hourly</option>
          <option value={360}>Every 6 hours</option>
          <option value={1440}>Daily</option>
          <option value={10080}>Weekly</option>
        </NativeSelect>
        <Button
          variant="outline"
          size="sm"
          disabled={!canWrite || phase !== "ready"}
          onClick={() =>
            void (audience.refresh_enabled
              ? submit({ kind: "schedule", refresh_enabled: false }, null)
              : review({ kind: "schedule", refresh_enabled: true }))
          }
        >
          {audience.refresh_enabled ? "Pause schedule" : "Resume schedule"}
        </Button>
        <Button
          size="sm"
          disabled={!canWrite || phase !== "ready"}
          onClick={() => void review({ kind: "refresh" })}
        >
          Refresh now
        </Button>
      </div>
      {note && (
        <p role="status" className="destination-muted">
          {note}
        </p>
      )}
      {!change && phase !== "ready" && (
        <p role="status" className="destination-muted">
          Updating the schedule…
        </p>
      )}
      {error && !change && (
        <p role="alert" className="destination-error">
          {error}
        </p>
      )}
      {!access.isPending && !access.isError && !canWrite && (
        <p className="destination-muted">
          View only. Activation write permission is required to refresh or
          change schedules.
        </p>
      )}
      {access.isError && (
        <p role="alert" className="destination-error">
          Activation permissions are unavailable.{" "}
          <button
            type="button"
            className="underline"
            onClick={() => void access.refetch()}
          >
            Check activation access again
          </button>
        </p>
      )}
      <Dialog
        open={!!change}
        onOpenChange={(open) => {
          if (!open) close()
        }}
      >
        <DialogContent
          className="destination-dialog"
          showCloseButton={!sending}
          finalFocus={() =>
            trigger.current?.isConnected ? trigger.current : null
          }
        >
          <DialogHeader>
            <DialogTitle>
              {change?.kind === "refresh"
                ? "Review audience refresh"
                : "Review schedule change"}
            </DialogTitle>
            <DialogDescription>{audience.name}</DialogDescription>
          </DialogHeader>
          <div className="destination-dialog-body">
            {phase === "loading" && (
              <p role="status">Loading audience and enabled destinations…</p>
            )}
            {context && (
              <>
                <p>
                  {change?.kind === "refresh"
                    ? "Recompute membership from the saved audience filters. Membership changes can automatically queue syncs to the enabled destinations below."
                    : `Save ${change?.kind === "schedule" && change.refresh_interval_minutes ? "a " + change.refresh_interval_minutes + " minute refresh interval" : "an enabled refresh schedule"}. Scheduled membership changes can automatically queue these destinations.`}
                </p>
                <p className="destination-muted">
                  Current snapshot: {context.audience.member_count} member
                  {context.audience.member_count === 1 ? "" : "s"}. Future
                  record, filter and destination changes affect what is eligible
                  when the refresh runs.
                </p>
                <section>
                  <h3 className="font-semibold">
                    Enabled destinations ({enabled.length})
                  </h3>
                  {enabled.length ? (
                    <ul className="destination-deliveries">
                      {enabled.map((d) => (
                        <li key={d.id}>
                          <strong>{d.name}</strong>
                          <p>
                            {d.destination_type} · {destinationTarget(d)}
                          </p>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <p>No destinations are currently enabled.</p>
                  )}
                </section>
                <p className="destination-muted">
                  A membership receipt confirms the refresh, not external
                  delivery. Inspect destination run histories for queued,
                  failed, skipped and successful deliveries.
                </p>
              </>
            )}
            {error && (
              <div role="alert" className="destination-error-block">
                <p>{error}</p>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={phase !== "ready"}
                  onClick={() => {
                    if (change) void review(change)
                  }}
                >
                  Refresh audience review
                </Button>
              </div>
            )}
            {sending && (
              <p role="status">
                Sending the request. Wait for the result before leaving.
              </p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" disabled={sending} onClick={close}>
              Cancel
            </Button>
            <Button
              disabled={
                !context || !canWrite || phase !== "ready" || needsReview
              }
              aria-busy={phase !== "ready"}
              onClick={() => {
                if (change && context) void submit(change, context)
              }}
            >
              {change?.kind === "refresh"
                ? "Confirm refresh"
                : "Confirm schedule"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
