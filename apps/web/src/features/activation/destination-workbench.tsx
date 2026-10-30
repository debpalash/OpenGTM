import { useLayoutEffect, useRef, useState } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { History, RefreshCw, Send, StopCircle } from "lucide-react"
import type { Audience, AudienceDestination, DestinationRun } from "@/lib/api"
import { getActiveWorkspace } from "@/lib/auth"
import { relativeTime } from "@/lib/record-overview"
import { queryKeys } from "@/lib/query-client"
import { RecordRequestError } from "@/lib/records"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import { useLeaveGuard } from "@/features/navigation/leave-guard-provider"
import { InboundSetup } from "./inbound-setup"
import { useActivationAccess } from "./use-activation-access"
import { audienceScope } from "./audience-subject"
import {
  ACTIVE_RUNS,
  actionUnavailable,
  destinationTarget,
  fetchActivationAccess,
  fetchDestinationContext,
  performDestinationAction,
  reviewFingerprint,
  validateRun,
  type DestinationAction,
  type DestinationContext
} from "./destination-client"
import {
  DestinationHistory,
  runHistoryKey,
  runLabel
} from "./destination-history"
import "./destination-workbench.css"

const titles = {
  sync: "Review audience sync",
  retry: "Review run retry",
  cancel: "Review sync cancellation"
}
const confirmLabels = {
  sync: "Confirm sync",
  retry: "Confirm retry",
  cancel: "Confirm cancellation"
}
export function DestinationWorkbench({
  destination,
  audience,
  maturity
}: {
  destination: AudienceDestination
  audience: Audience
  maturity: string
}) {
  const access = useActivationAccess(),
    canWrite = access.data?.can_write === true && !access.isError
  const client = useQueryClient(),
    workspace = getActiveWorkspace()
  const [mode, setMode] = useState<"review" | "history" | null>(null)
  const [action, setAction] = useState<DestinationAction>("sync")
  const [targetRun, setTargetRun] = useState<string | undefined>()
  const [context, setContext] = useState<DestinationContext | null>(null)
  const [stage, setStage] = useState<
    "loading" | "ready" | "checking" | "sending"
  >("ready")
  const [error, setError] = useState("")
  const [needsReview, setNeedsReview] = useState(false)
  const [receipt, setReceipt] = useState<DestinationRun | null>(null)
  const [selectedRun, setSelectedRun] = useState<string | null>(null)
  const pending = useRef(false),
    controller = useRef<AbortController | null>(null),
    alive = useRef(true)
  const historyTrigger = useRef<HTMLButtonElement>(null)
  const reviewTrigger = useRef<HTMLElement | null>(null)
  const receiptHeading = useRef<HTMLHeadingElement>(null)
  const sending = stage === "sending"
  useLeaveGuard({
    dirty: false,
    busy: sending,
    busyLabel: `${action === "sync" ? "Sync" : action === "retry" ? "Retry" : "Cancellation"} request in progress`
  })
  useLayoutEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
      controller.current?.abort()
    }
  }, [])
  useLayoutEffect(() => {
    if (mode !== "history") return
    const frame = requestAnimationFrame(() =>
      receiptHeading.current?.focus({ preventScroll: true })
    )
    return () => cancelAnimationFrame(frame)
  }, [mode])
  let latest: DestinationRun | null = null
  try {
    if (destination.latest_run)
      latest = validateRun(destination.latest_run, destination.id)
  } catch {
    /* History will present the read error without inventing a state. */
  }
  async function loadReview(kind: DestinationAction, runID?: string) {
    if (pending.current) return
    if (mode === null)
      reviewTrigger.current = document.activeElement as HTMLElement
    controller.current?.abort()
    const abort = new AbortController()
    controller.current = abort
    setAction(kind)
    setTargetRun(runID)
    setMode("review")
    setStage("loading")
    setError("")
    setContext(null)
    setNeedsReview(false)
    try {
      const current = await fetchDestinationContext(
        destination.id,
        audience.id,
        abort.signal
      )
      if (!abort.signal.aborted && alive.current) setContext(current)
    } catch (cause) {
      if (!abort.signal.aborted && alive.current)
        setError(
          cause instanceof Error
            ? cause.message
            : "Review could not be loaded. Try again."
        )
    } finally {
      if (!abort.signal.aborted && alive.current) setStage("ready")
    }
  }
  function close() {
    if (sending) return
    controller.current?.abort()
    setMode(null)
  }
  function history() {
    if (mode === null)
      reviewTrigger.current = document.activeElement as HTMLElement
    setSelectedRun(receipt?.id || latest?.id || null)
    setMode("history")
  }
  async function confirm() {
    if (
      pending.current ||
      !context ||
      !canWrite ||
      needsReview ||
      actionUnavailable(context, action, targetRun)
    )
      return
    pending.current = true
    setStage("checking")
    setError("")
    const abort = new AbortController()
    controller.current = abort
    let submitted = false
    try {
      const [current, permission] = await Promise.all([
        fetchDestinationContext(destination.id, audience.id, abort.signal),
        fetchActivationAccess(abort.signal)
      ])
      if (abort.signal.aborted || !alive.current) return
      if (!permission.can_write) {
        void client.invalidateQueries({
          queryKey: ["activation-access", workspace]
        })
        throw new Error(
          "Your activation permission changed. This action was not sent."
        )
      }
      if (
        reviewFingerprint(current, action) !==
        reviewFingerprint(context, action)
      )
        throw new Error(
          "The destination, audience or run changed after review. Refresh this review before continuing."
        )
      const unavailable = actionUnavailable(current, action, targetRun)
      if (unavailable) throw new Error(unavailable)
      await client.cancelQueries({
        queryKey: runHistoryKey(workspace, destination.id)
      })
      if (abort.signal.aborted || !alive.current) return
      setStage("sending")
      submitted = true
      const result = await performDestinationAction(
        action,
        destination.id,
        targetRun
      )
      if (!alive.current) return
      // Keep the confirmed write receipt before performing any follow-up reads.
      setReceipt(result)
      setSelectedRun(result.id)
      const destinationKey = [
        ...queryKeys.audienceDestinations(audience.id),
        workspace
      ]
      await client.cancelQueries({ queryKey: destinationKey, exact: true })
      if (!alive.current) return
      client.setQueryData<AudienceDestination[]>(destinationKey, (current) =>
        current?.map((item) =>
          item.id === destination.id ? { ...item, latest_run: result } : item
        )
      )
      await client.resetQueries({
        queryKey: runHistoryKey(workspace, destination.id),
        exact: true
      })
      if (!alive.current) return
      setMode("history")
      void client.invalidateQueries({
        queryKey: queryKeys.audienceDestinations(audience.id)
      })
    } catch (cause) {
      if (abort.signal.aborted || !alive.current) return
      const known =
        cause instanceof RecordRequestError &&
        cause.status >= 400 &&
        cause.status < 500
      setError(
        cause instanceof Error
          ? `${cause.message}${submitted && !known ? " The request may have reached the server. Check run history before trying again." : ""}`
          : "The result could not be confirmed. Check run history before trying again."
      )
      setNeedsReview(true)
    } finally {
      pending.current = false
      if (alive.current && !abort.signal.aborted) setStage("ready")
    }
  }
  const unavailable = context
    ? actionUnavailable(context, action, targetRun)
    : null
  const busy = stage !== "ready"
  return (
    <section
      className="destination-workbench"
      aria-label={`${destination.name} destination`}
    >
      <div className="destination-card-heading">
        <strong>{destination.name}</strong>
        <Badge variant="outline">{destination.destination_type}</Badge>
        <Badge variant="secondary">{maturity}</Badge>
        <span className="destination-muted">
          {destination.enabled ? destination.health_status : "Disabled"}
        </span>
      </div>
      <p className="destination-card-status">
        {latest
          ? `${runLabel(latest.status)} · ${latest.succeeded} succeeded · ${latest.failed} failed · ${latest.skipped} skipped`
          : destination.latest_run
            ? "Run summary unavailable. Open run history."
            : "No sync runs yet."}
      </p>
      {destination.last_error && (
        <p className="destination-error">{destination.last_error}</p>
      )}
      <div className="destination-card-actions">
        <Button
          ref={historyTrigger}
          variant="outline"
          size="sm"
          onClick={history}
        >
          <History aria-hidden="true" /> Run history
        </Button>
        {["hubspot", "salesforce"].includes(destination.destination_type) && (
          <InboundSetup destination={destination} canWrite={canWrite} />
        )}
        {latest && ["pending", "running"].includes(latest.status) && (
          <Button
            variant="outline"
            size="sm"
            disabled={!canWrite}
            onClick={() => void loadReview("cancel", latest!.id)}
          >
            <StopCircle aria-hidden="true" /> Cancel sync
          </Button>
        )}
        {latest?.status === "cancelling" && (
          <span className="destination-muted">Cancellation requested</span>
        )}
        {latest &&
          ["failed", "completed_with_errors"].includes(latest.status) && (
            <Button
              variant="outline"
              size="sm"
              disabled={!canWrite || !destination.enabled}
              onClick={() => void loadReview("retry", latest!.id)}
            >
              <RefreshCw aria-hidden="true" /> Review retry
            </Button>
          )}
        {(!latest || !ACTIVE_RUNS.has(latest.status)) && (
          <Button
            variant="outline"
            size="sm"
            disabled={
              !canWrite ||
              !destination.enabled ||
              (!!destination.latest_run && !latest)
            }
            onClick={() => void loadReview("sync")}
          >
            <Send aria-hidden="true" /> Sync audience
          </Button>
        )}
      </div>
      <Dialog
        open={mode !== null}
        onOpenChange={(open) => {
          if (!open) close()
        }}
      >
        <DialogContent
          className="destination-dialog"
          showCloseButton={!sending}
          finalFocus={() =>
            reviewTrigger.current?.isConnected
              ? reviewTrigger.current
              : historyTrigger.current
          }
        >
          <DialogHeader>
            <DialogTitle ref={receiptHeading} tabIndex={-1}>
              {mode === "review" ? titles[action] : "Destination run history"}
            </DialogTitle>
            <DialogDescription>
              {destination.name} · {destination.destination_type}
            </DialogDescription>
          </DialogHeader>
          <div className="destination-dialog-body">
            {mode === "history" ? (
              <DestinationHistory
                destinationID={destination.id}
                receipt={receipt}
                selectedID={selectedRun}
                onSelect={setSelectedRun}
              />
            ) : (
              <>
                {stage === "loading" && (
                  <p role="status">Loading current destination and audience…</p>
                )}
                {context && (
                  <>
                    <dl className="destination-review-facts">
                      <div>
                        <dt>Audience</dt>
                        <dd>
                          {context.audience.name} ·{" "}
                          {audienceScope(context.audience.filters)}
                        </dd>
                      </div>
                      <div>
                        <dt>Current membership</dt>
                        <dd>
                          {context.audience.member_count} record
                          {context.audience.member_count === 1 ? "" : "s"}
                          {context.audience.refreshed_at
                            ? " · refreshed " +
                              relativeTime(
                                context.audience.refreshed_at
                              ).toLowerCase()
                            : " · not refreshed yet"}
                        </dd>
                      </div>
                      <div>
                        <dt>Destination</dt>
                        <dd>
                          {context.destination.name} ·{" "}
                          {destinationTarget(context.destination)}
                        </dd>
                      </div>
                    </dl>
                    {action === "sync" && (
                      <p>
                        This queues a sync using membership and destination
                        settings available when it starts. Changes before
                        execution can affect the records and fields sent.
                      </p>
                    )}
                    {action === "retry" && (
                      <p>
                        Retry this run against the current audience. Successful
                        delivery keys are skipped. The last recorded failed
                        count ({context.destination.latest_run?.failed}) is not
                        a fixed recipient list.
                      </p>
                    )}
                    {action === "cancel" && (
                      <p>
                        Stop this run at the next delivery boundary. Completed
                        deliveries remain; in-flight requests may finish before
                        cancellation takes effect.
                      </p>
                    )}
                    {targetRun && (
                      <code className="destination-id">Run {targetRun}</code>
                    )}
                    {action !== "cancel" && (
                      <section className="destination-field-review">
                        <h3>Field mapping</h3>
                        {context.destination.destination_type.endsWith(
                          "_ads"
                        ) ? (
                          <p>
                            Advertising sync uses supported hashed identifiers
                            from member data. Custom field mapping does not
                            change which advertising identifiers are selected.
                          </p>
                        ) : Object.keys(context.destination.field_map)
                            .length ? (
                          <dl>
                            {Object.entries(context.destination.field_map).map(
                              ([source, target]) => (
                                <div key={source}>
                                  <dt>{source}</dt>
                                  <dd>{target}</dd>
                                </div>
                              )
                            )}
                          </dl>
                        ) : (
                          <p>
                            Without a custom mapping, the integration can use
                            all saved member fields. Each integration determines
                            which fields it sends.
                          </p>
                        )}
                      </section>
                    )}
                    {maturity !== "supported" && maturity !== "beta" && (
                      <p className="destination-muted">
                        Integration status could not be verified. Review
                        availability before using this destination.
                      </p>
                    )}
                    {maturity === "beta" && (
                      <p className="destination-muted">
                        Beta integration. Verify a small audience before
                        production use.
                      </p>
                    )}
                  </>
                )}
                {unavailable && (
                  <p role="status" className="destination-error">
                    {unavailable}
                  </p>
                )}
                {error && (
                  <div role="alert" className="destination-error-block">
                    <p>{error}</p>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={busy}
                      onClick={() => void loadReview(action, targetRun)}
                    >
                      Refresh review
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={busy}
                      onClick={history}
                    >
                      Check run history
                    </Button>
                  </div>
                )}
                {!canWrite && (
                  <p className="destination-muted">
                    Activation write permission is required in this workspace.
                  </p>
                )}
                {stage === "checking" && (
                  <p role="status">
                    Checking that the reviewed scope is still current…
                  </p>
                )}
                {sending && (
                  <p role="status">
                    Sending the request. Wait for its result before leaving.
                  </p>
                )}
              </>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" disabled={sending} onClick={close}>
              {mode === "review" ? "Cancel" : "Close history"}
            </Button>
            {mode === "review" && (
              <Button
                className="destination-confirm"
                disabled={
                  busy || !context || !canWrite || !!unavailable || needsReview
                }
                aria-busy={busy}
                onClick={() => void confirm()}
              >
                {busy && (
                  <RefreshCw
                    aria-hidden="true"
                    className="size-3 animate-spin"
                  />
                )}
                {confirmLabels[action]}
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}
