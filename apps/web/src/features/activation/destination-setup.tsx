import { useId, useLayoutEffect, useRef, useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Plus } from "lucide-react"
import type { Audience, AudienceDestination, DestinationType } from "@/lib/api"
import { getActiveWorkspace } from "@/lib/auth"
import { queryKeys } from "@/lib/query-client"
import { RecordRequestError } from "@/lib/records"
import { relativeTime } from "@/lib/record-overview"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { NativeSelect } from "@/components/ui/native-select"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import {
  useLeaveConfirmation,
  useLeaveGuard
} from "@/features/navigation/leave-guard-provider"
import { audienceScope } from "./audience-subject"
import {
  destinationTarget,
  fetchActivationAccess,
  fetchAudienceActivationContext,
  type AudienceActivationContext
} from "./destination-client"
import { useActivationAccess } from "./use-activation-access"
import {
  SETUP_TYPES,
  SetupValidationError,
  createReviewedDestination,
  creationFingerprint,
  destinationInput,
  draftValues,
  fetchSetupCatalog,
  initialDestinationDraft,
  sameDestination,
  type CreateDestinationInput,
  type DestinationDraft
} from "./destination-setup-client"
import "./destination-workbench.css"
import "./destination-setup.css"

interface Review {
  context: AudienceActivationContext
  type: DestinationType
  input: CreateDestinationInput
}
export function DestinationSetup({ audience }: { audience: Audience }) {
  const workspace = getActiveWorkspace(),
    client = useQueryClient(),
    leave = useLeaveConfirmation()
  const access = useActivationAccess(),
    canWrite = access.data?.can_write === true && !access.isError
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<DestinationDraft>(initialDestinationDraft)
  const [step, setStep] = useState<"configure" | "review" | "receipt">(
    "configure"
  )
  const [phase, setPhase] = useState<
    "ready" | "loading" | "checking" | "sending" | "recovering"
  >("ready")
  const [review, setReview] = useState<Review | null>(null)
  const [receipt, setReceipt] = useState<{
    destination: AudienceDestination
    recovered: boolean
  } | null>(null)
  const [error, setError] = useState("")
  const [fields, setFields] = useState<Record<string, string>>({})
  const [needsReview, setNeedsReview] = useState(false)
  const [uncertain, setUncertain] = useState(false)
  const [enabledConfirmed, setEnabledConfirmed] = useState(false)
  const [consentConfirmed, setConsentConfirmed] = useState(false)
  const [showEndpoint, setShowEndpoint] = useState(false)
  const pending = useRef(false),
    writing = useRef(false),
    alive = useRef(true),
    controller = useRef<AbortController | null>(null)
  const focusedStep = useRef<typeof step | null>(null)
  const focusedReview = useRef<Review | null>(null)
  const attempted = useRef<CreateDestinationInput | null>(null)
  const trigger = useRef<HTMLButtonElement>(null),
    heading = useRef<HTMLHeadingElement>(null),
    title = useRef<HTMLHeadingElement>(null),
    name = useRef<HTMLInputElement>(null),
    form = useRef<HTMLFormElement>(null)
  const id = useId(),
    definition = SETUP_TYPES[draft.type],
    values = draftValues(draft)
  const dirty =
    open &&
    !receipt &&
    JSON.stringify(draft) !== JSON.stringify(initialDestinationDraft())
  const sending = phase === "sending",
    working = phase !== "ready"
  useLeaveGuard({
    dirty,
    busy: sending,
    busyLabel: "Destination creation in progress",
    message: uncertain
      ? "The destination request may already be saved. Discarding this draft does not undo it; check saved destinations before sending again."
      : "Your destination setup has unsaved configuration. Keep editing or discard it to continue."
  })
  const catalog = useQuery({
    queryKey: ["destination-setup-catalog", workspace],
    queryFn: ({ signal }) => fetchSetupCatalog(signal),
    enabled: open,
    staleTime: 15000
  })
  useLayoutEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
      controller.current?.abort()
    }
  }, [])
  useLayoutEffect(() => {
    const previous = focusedStep.current
    const previousReview = focusedReview.current
    focusedStep.current = open ? step : null
    focusedReview.current = open ? review : null
    // Base UI owns initial opening focus. A second queued focus can steal
    // keystrokes after the operator has already moved to another field.
    if (
      !open ||
      previous === null ||
      (previous === step && !(step === "review" && previousReview !== review))
    )
      return
    const target = step === "configure" ? name.current : title.current
    target?.focus({ preventScroll: true })
  }, [open, step, review])
  useLayoutEffect(() => {
    if (open && step === "configure" && Object.keys(fields).length)
      form.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }, [open, step, fields])
  function start() {
    if (!canWrite || pending.current) return
    setDraft(initialDestinationDraft())
    setStep("configure")
    setPhase("ready")
    setReview(null)
    setReceipt(null)
    setError("")
    setFields({})
    setNeedsReview(false)
    setUncertain(false)
    attempted.current = null
    setEnabledConfirmed(false)
    setConsentConfirmed(false)
    setShowEndpoint(false)
    setOpen(true)
  }
  function stopRead() {
    controller.current?.abort()
    controller.current = null
    pending.current = false
  }
  async function close() {
    if (writing.current) return
    if (dirty && !(await leave.request("Discard destination setup"))) return
    if (!alive.current || writing.current) return
    stopRead()
    setPhase("ready")
    setOpen(false)
  }
  function edit() {
    if (working) return
    setStep("configure")
    setError("")
    setReview(null)
    setNeedsReview(false)
    setEnabledConfirmed(false)
    setConsentConfirmed(false)
    setShowEndpoint(false)
    // A subsequent review must check the saved list before another POST.
  }
  async function loadReview() {
    if (pending.current || !canWrite) return
    let input: CreateDestinationInput
    try {
      input = destinationInput(audience.id, draft)
      setFields({})
    } catch (cause) {
      if (cause instanceof SetupValidationError) setFields(cause.fields)
      setError(
        cause instanceof Error
          ? cause.message
          : "Check the destination configuration."
      )
      return
    }
    const abort = new AbortController()
    controller.current = abort
    pending.current = true
    setPhase("loading")
    setError("")
    setEnabledConfirmed(false)
    setConsentConfirmed(false)
    try {
      const [context, types, permission] = await Promise.all([
        fetchAudienceActivationContext(audience.id, abort.signal),
        fetchSetupCatalog(abort.signal),
        fetchActivationAccess(abort.signal)
      ])
      if (abort.signal.aborted || !alive.current) return
      if (!permission.can_write) {
        void access.refetch()
        throw new Error(
          "Your activation permission changed. Nothing was submitted."
        )
      }
      const type = types.find((t) => t.id === input.destination_type)
      if (!type)
        throw new Error(
          "This destination type is no longer available. Choose an available type."
        )
      if (context.destinations.some((d) => d.name === input.name))
        throw new Error(
          "A destination with this name is already saved for this audience. Check its configuration before choosing another name."
        )
      setReview({ context, type, input })
      setNeedsReview(false)
      setUncertain(false)
      attempted.current = null
      setStep("review")
    } catch (cause) {
      if (!abort.signal.aborted && alive.current) {
        setError(
          (cause instanceof Error
            ? cause.message
            : "The review could not be loaded.") +
            " No creation request was sent."
        )
        setNeedsReview(true)
      }
    } finally {
      if (controller.current === abort) {
        pending.current = false
        if (alive.current) setPhase("ready")
      }
    }
  }
  async function confirmed(
    destination: AudienceDestination,
    recovered: boolean,
    baseline: AudienceActivationContext
  ) {
    // The write receipt is authoritative before any optional read can fail.
    setReceipt({ destination, recovered })
    setStep("receipt")
    setUncertain(false)
    setError("")
    try {
      await client.cancelQueries({
        queryKey: queryKeys.audienceDestinations(audience.id)
      })
      if (!alive.current) return
      client.setQueryData<AudienceDestination[]>(
        queryKeys.audienceDestinations(audience.id),
        (current) => {
          const list = current ?? baseline.destinations
          return [...list.filter((d) => d.id !== destination.id), destination]
        }
      )
      void client.invalidateQueries({
        queryKey: queryKeys.audienceDestinations(audience.id)
      })
    } catch {
      if (alive.current)
        setError(
          "Configuration is saved. The destination list could not be refreshed; keep this receipt and try the list again."
        )
    }
  }
  async function submit() {
    if (
      pending.current ||
      !review ||
      !canWrite ||
      needsReview ||
      uncertain ||
      (review.input.enabled && !enabledConfirmed) ||
      (review.input.destination_type.endsWith("_ads") && !consentConfirmed)
    )
      return
    const baseline = review,
      abort = new AbortController()
    controller.current = abort
    pending.current = true
    setPhase("checking")
    setError("")
    let sent = false
    try {
      const [context, types, permission] = await Promise.all([
        fetchAudienceActivationContext(audience.id, abort.signal),
        fetchSetupCatalog(abort.signal),
        fetchActivationAccess(abort.signal)
      ])
      if (abort.signal.aborted || !alive.current) return
      if (!permission.can_write) {
        void access.refetch()
        throw new Error(
          "Your activation permission changed. The destination was not submitted."
        )
      }
      const type = types.find((t) => t.id === baseline.input.destination_type)
      if (
        !type ||
        creationFingerprint(context, type, baseline.input) !==
          creationFingerprint(baseline.context, baseline.type, baseline.input)
      )
        throw new Error(
          "Audience scope, refresh schedule or destination availability changed. Refresh the review before continuing; nothing was submitted."
        )
      if (context.destinations.some((d) => d.name === baseline.input.name))
        throw new Error(
          "A destination with this name is now saved. Check saved destinations before submitting another request."
        )
      const input = { ...baseline.input, config: { ...baseline.input.config } }
      if (input.destination_type.endsWith("_ads"))
        input.config.consent_attested = consentConfirmed
      attempted.current = input
      sent = true
      writing.current = true
      setPhase("sending")
      const saved = await createReviewedDestination(input)
      if (alive.current) await confirmed(saved, false, context)
    } catch (cause) {
      if (!alive.current || abort.signal.aborted) return
      const unknown =
        sent &&
        !(
          cause instanceof RecordRequestError &&
          [401, 403, 404, 409, 422].includes(cause.status)
        )
      setUncertain(unknown)
      setNeedsReview(true)
      setError(
        (cause instanceof Error
          ? cause.message
          : "Destination creation could not be confirmed.") +
          (unknown
            ? " The destination may already be saved. Check saved destinations before sending again. Your draft is kept; no automatic retry was made."
            : sent
              ? " The server refused the request. Your draft is kept."
              : " No creation request was sent. Your draft is kept.")
      )
    } finally {
      writing.current = false
      if (controller.current === abort) {
        pending.current = false
        if (alive.current) setPhase("ready")
      }
    }
  }
  async function recover() {
    if (pending.current || !attempted.current) return
    const input = attempted.current,
      abort = new AbortController()
    controller.current = abort
    pending.current = true
    setPhase("recovering")
    setError("")
    try {
      const context = await fetchAudienceActivationContext(
        audience.id,
        abort.signal
      )
      if (abort.signal.aborted || !alive.current) return
      const match = context.destinations.find((d) => sameDestination(d, input))
      if (match) await confirmed(match, true, context)
      else
        setError(
          context.destinations.some((d) => d.name === input.name)
            ? "A destination with this name exists, but its configuration differs from this draft. Review the saved destination; this request is not confirmed."
            : "No matching destination appeared in this read. The earlier request may still be processing. Check again, or edit and make a fresh review before another submission."
        )
    } catch (cause) {
      if (!abort.signal.aborted && alive.current)
        setError(
          (cause instanceof Error
            ? cause.message
            : "Saved destinations could not be checked.") +
            " The earlier request is still unconfirmed; your draft is kept."
        )
    } finally {
      if (controller.current === abort) {
        pending.current = false
        if (alive.current) setPhase("ready")
      }
    }
  }
  const reviewed = review?.input
  return (
    <>
      <div className="destination-setup-heading">
        <div>
          <h3 ref={heading} tabIndex={-1}>
            Connect a destination
          </h3>
          <p>
            Review the target, audience and future sync behavior before saving.
          </p>
        </div>
        <Button
          ref={trigger}
          size="sm"
          variant="outline"
          disabled={!canWrite}
          onClick={start}
        >
          <Plus aria-hidden="true" />
          Add destination
        </Button>
      </div>
      <Dialog
        open={open}
        onOpenChangeComplete={(isOpen) => {
          if (isOpen || open || !alive.current) return
          setDraft(initialDestinationDraft())
          setReview(null)
          setReceipt(null)
          attempted.current = null
          setUncertain(false)
          setFields({})
          setError("")
        }}
        onOpenChange={(next, details) => {
          if (!next) {
            details.cancel()
            void close()
          }
        }}
      >
        <DialogContent
          className="destination-dialog destination-setup-dialog"
          showCloseButton={!sending}
          finalFocus={() =>
            trigger.current?.isConnected && !trigger.current.disabled
              ? trigger.current
              : heading.current
          }
          initialFocus={name}
        >
          <DialogHeader>
            <DialogTitle ref={title} tabIndex={-1}>
              {step === "receipt"
                ? receipt?.recovered
                  ? "Matching saved destination found"
                  : "Destination saved"
                : step === "review"
                  ? "Review new destination"
                  : "Configure destination"}
            </DialogTitle>
            <DialogDescription>
              {step === "receipt"
                ? "This receipt confirms saved configuration. Delivery is tracked separately in destination run history."
                : `For ${review?.context.audience.name ?? audience.name}. Configuration stays in memory until saved or discarded.`}
            </DialogDescription>
          </DialogHeader>
          {step === "configure" ? (
            <form
              id={`${id}-form`}
              ref={form}
              noValidate
              onSubmit={(event) => {
                event.preventDefault()
                void loadReview()
              }}
              className="destination-dialog-body"
            >
              {catalog.isFetching && (
                <p role="status">Checking destination availability…</p>
              )}
              {catalog.isError && (
                <div className="destination-error-block" role="alert">
                  <p>
                    Destination availability could not be verified. Your draft
                    is kept.
                  </p>
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={catalog.isFetching}
                    aria-busy={catalog.isFetching}
                    onClick={() => void catalog.refetch()}
                  >
                    Try catalog again
                  </Button>
                </div>
              )}
              {catalog.data?.length === 0 && !catalog.isError && (
                <div role="status">
                  <p>
                    No destination types are currently available. Your draft is
                    kept.
                  </p>
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={catalog.isFetching}
                    aria-busy={catalog.isFetching}
                    onClick={() => void catalog.refetch()}
                  >
                    Refresh catalog
                  </Button>
                </div>
              )}
              {!canWrite && (
                <p role="status">
                  Activation write permission is unavailable. Your draft is
                  kept; you can close or discard it.
                </p>
              )}
              <fieldset
                disabled={working || !canWrite}
                className="destination-setup-fields"
              >
                <div className="destination-setup-field destination-setup-wide">
                  <label htmlFor={`${id}-name`}>Destination name</label>
                  <Input
                    id={`${id}-name`}
                    ref={name}
                    value={draft.name}
                    onChange={(event) =>
                      setDraft((d) => ({ ...d, name: event.target.value }))
                    }
                    placeholder="Destination name"
                    required
                    aria-invalid={!!fields.name}
                    aria-describedby={`${id}-name-help`}
                  />
                  <p
                    id={`${id}-name-help`}
                    className={
                      fields.name ? "destination-error" : "destination-muted"
                    }
                  >
                    {fields.name ||
                      "A unique name within this audience; up to 200 characters."}
                  </p>
                </div>
                <div className="destination-setup-field">
                  <label htmlFor={`${id}-type`}>Destination type</label>
                  <NativeSelect
                    id={`${id}-type`}
                    value={draft.type}
                    disabled={!catalog.data?.length || catalog.isError}
                    onChange={(event) => {
                      setDraft((d) => ({
                        ...d,
                        type: event.target.value as DestinationDraft["type"]
                      }))
                      setFields({})
                      setError("")
                    }}
                  >
                    {Object.entries(SETUP_TYPES).map(([kind, type]) => {
                      const available = catalog.data?.some((t) => t.id === kind)
                      return (
                        <option value={kind} key={kind} disabled={!available}>
                          {type.label}
                          {catalog.data && !available ? " (unavailable)" : ""}
                        </option>
                      )
                    })}
                  </NativeSelect>
                </div>
                <div className="destination-setup-field">
                  <label htmlFor={`${id}-enabled`}>Delivery availability</label>
                  <NativeSelect
                    id={`${id}-enabled`}
                    value={draft.enabled ? "enabled" : "disabled"}
                    onChange={(event) =>
                      setDraft((d) => ({
                        ...d,
                        enabled: event.target.value === "enabled"
                      }))
                    }
                  >
                    <option value="enabled">
                      Enabled — manual and automatic sync
                    </option>
                    <option value="disabled">
                      Disabled — configuration only
                    </option>
                  </NativeSelect>
                </div>
                {definition.fields.map((field) => {
                  const fieldID = `${id}-${field.key}`,
                    help = `${fieldID}-help`
                  const props = {
                    id: fieldID,
                    value: values[field.key]!,
                    required: !field.optional,
                    "aria-invalid": !!fields[field.key],
                    "aria-describedby": help,
                    onChange: (
                      event: React.ChangeEvent<
                        | HTMLInputElement
                        | HTMLTextAreaElement
                        | HTMLSelectElement
                      >
                    ) =>
                      setDraft((d) => ({
                        ...d,
                        configs: {
                          ...d.configs,
                          [d.type]: {
                            ...d.configs[d.type],
                            [field.key]: event.target.value
                          }
                        }
                      }))
                  }
                  return (
                    <div
                      className={`destination-setup-field ${field.multiline ? "destination-setup-wide" : ""}`}
                      key={`${draft.type}:${field.key}`}
                    >
                      <label htmlFor={fieldID}>
                        {field.label}
                        {field.optional ? " (optional)" : ""}
                      </label>
                      {field.choices ? (
                        <NativeSelect {...props}>
                          {field.choices.map((choice) => (
                            <option key={choice.value} value={choice.value}>
                              {choice.label}
                            </option>
                          ))}
                        </NativeSelect>
                      ) : field.multiline ? (
                        <textarea {...props} rows={3} />
                      ) : (
                        <Input
                          {...props}
                          type={field.key === "url" ? "url" : "text"}
                          placeholder={field.placeholder}
                          autoComplete="off"
                          spellCheck={false}
                        />
                      )}
                      <p
                        id={help}
                        className={
                          fields[field.key]
                            ? "destination-error"
                            : "destination-muted"
                        }
                      >
                        {fields[field.key] ||
                          field.help ||
                          "Required for this destination."}
                      </p>
                    </div>
                  )
                })}
              </fieldset>
              <p className="destination-muted">{definition.help}</p>
              <p className="destination-muted">
                Enabled destinations can be queued when audience membership
                changes, including scheduled refreshes and Refresh now. Creation
                saves configuration; it does not queue a sync of existing
                members.
              </p>
              {uncertain && (
                <p role="status" className="destination-error-block">
                  The earlier request is unconfirmed. A fresh review checks the
                  saved list before another submission. Discarding this draft
                  does not undo a saved destination.
                </p>
              )}
              {error && (
                <p role="alert" className="destination-error">
                  {error}
                </p>
              )}
            </form>
          ) : step === "review" && review && reviewed ? (
            <div className="destination-dialog-body">
              <dl className="destination-review-facts">
                <div>
                  <dt>Destination</dt>
                  <dd>
                    {reviewed.name} ·{" "}
                    {SETUP_TYPES[reviewed.destination_type].label}
                  </dd>
                </div>
                <div>
                  <dt>Audience</dt>
                  <dd>
                    {review.context.audience.name} ·{" "}
                    {audienceScope(review.context.audience.filters)}
                  </dd>
                </div>
                <div>
                  <dt>Membership snapshot</dt>
                  <dd>
                    {review.context.audience.member_count.toLocaleString()}{" "}
                    members ·{" "}
                    {review.context.audience.refreshed_at
                      ? relativeTime(review.context.audience.refreshed_at)
                      : "Never refreshed"}
                  </dd>
                </div>
                <div>
                  <dt>Refresh schedule</dt>
                  <dd>
                    {review.context.audience.refresh_enabled
                      ? `Every ${review.context.audience.refresh_interval_minutes} minutes`
                      : "Paused"}
                  </dd>
                </div>
                <div>
                  <dt>Target</dt>
                  <dd>
                    {destinationTarget({
                      destination_type: reviewed.destination_type,
                      config: reviewed.config
                    } as AudienceDestination)}
                    {["webhook", "warehouse_http"].includes(
                      reviewed.destination_type
                    ) && (
                      <div className="destination-endpoint-review">
                        <label htmlFor={`${id}-endpoint`}>
                          Full endpoint URL
                        </label>
                        <div>
                          <Input
                            id={`${id}-endpoint`}
                            type={showEndpoint ? "text" : "password"}
                            value={String(reviewed.config.url)}
                            readOnly
                            autoComplete="off"
                            spellCheck={false}
                          />
                          <Button
                            size="sm"
                            variant="outline"
                            aria-pressed={showEndpoint}
                            onClick={() => setShowEndpoint((value) => !value)}
                          >
                            {showEndpoint
                              ? "Hide full endpoint"
                              : "Show full endpoint"}
                          </Button>
                        </div>
                        <p className="destination-muted">
                          Show the full endpoint to verify its path and query.
                        </p>
                      </div>
                    )}
                  </dd>
                </div>
                <div>
                  <dt>Delivery availability</dt>
                  <dd>
                    {reviewed.enabled
                      ? "Enabled — manual and automatic sync"
                      : "Disabled — delivery unavailable"}
                  </dd>
                </div>
                <div>
                  <dt>Integration status</dt>
                  <dd>
                    {review.type.maturity === "supported"
                      ? "Supported on this deployment"
                      : "Beta — no current controlled-live certification"}
                  </dd>
                </div>
              </dl>
              <section className="destination-field-review">
                <h3>Destination settings</h3>
                <dl>
                  {SETUP_TYPES[reviewed.destination_type].fields
                    .filter((field) => field.key !== "url")
                    .map((field) => (
                      <div key={field.key}>
                        <dt>{field.label}</dt>
                        <dd>
                          {Array.isArray(reviewed.config[field.key])
                            ? (reviewed.config[field.key] as string[]).join(
                                ", "
                              )
                            : (field.choices?.find(
                                (choice) =>
                                  choice.value === reviewed.config[field.key]
                              )?.label ??
                              String(
                                reviewed.config[field.key] ||
                                  (field.optional ? "Default template" : "—")
                              ))}
                        </dd>
                      </div>
                    ))}
                </dl>
                <p className="destination-muted">
                  {SETUP_TYPES[reviewed.destination_type].help}
                </p>
              </section>
              <p>
                {reviewed.enabled
                  ? "Saving enables this destination for manual sync and future membership changes. Scheduled refreshes and Refresh now can queue it automatically; a paused schedule can be resumed later. Creation itself does not queue delivery of current members."
                  : "This destination will be saved disabled. Manual and automatic delivery are unavailable until it is enabled."}{" "}
                Execution uses the settings and membership available when a run
                starts.
              </p>
              {reviewed.enabled && (
                <label className="destination-setup-check">
                  <input
                    type="checkbox"
                    checked={enabledConfirmed}
                    disabled={working || !canWrite}
                    onChange={(event) =>
                      setEnabledConfirmed(event.target.checked)
                    }
                  />
                  <span>
                    I understand future audience membership changes can
                    automatically send records to this target.
                  </span>
                </label>
              )}
              {reviewed.destination_type.endsWith("_ads") && (
                <label className="destination-setup-check">
                  <input
                    type="checkbox"
                    checked={consentConfirmed}
                    disabled={working || !canWrite}
                    onChange={(event) =>
                      setConsentConfirmed(event.target.checked)
                    }
                  />
                  <span>
                    I confirm this audience has permission for advertising use.
                  </span>
                </label>
              )}
              {!canWrite && (
                <p role="status">
                  Activation write permission is unavailable. Nothing further
                  will be submitted.
                </p>
              )}
              {working && (
                <p role="status">
                  {sending
                    ? "Creating destination. Wait for the result before leaving."
                    : phase === "recovering"
                      ? "Checking saved destinations…"
                      : "Checking current audience, destination availability and permission…"}
                </p>
              )}
              {error && (
                <div className="destination-error-block" role="alert">
                  <p>{error}</p>
                  {attempted.current && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={working}
                      onClick={() => void recover()}
                    >
                      Check saved destinations
                    </Button>
                  )}
                </div>
              )}
              {needsReview && !uncertain && (
                <Button
                  variant="outline"
                  disabled={working || !canWrite}
                  onClick={() => void loadReview()}
                >
                  Refresh review
                </Button>
              )}
            </div>
          ) : receipt ? (
            <div className="destination-dialog-body">
              <dl className="destination-review-facts">
                <div>
                  <dt>Destination</dt>
                  <dd>{receipt.destination.name}</dd>
                </div>
                <div>
                  <dt>Saved identity</dt>
                  <dd>{receipt.destination.id}</dd>
                </div>
                <div>
                  <dt>Target</dt>
                  <dd>{destinationTarget(receipt.destination)}</dd>
                </div>
                <div>
                  <dt>Availability</dt>
                  <dd>
                    {receipt.destination.enabled
                      ? "Enabled — future membership changes may queue delivery"
                      : "Disabled — delivery unavailable"}
                  </dd>
                </div>
              </dl>
              {receipt.recovered && (
                <p>
                  A saved destination matches the reviewed configuration. This
                  confirms the saved state; it does not identify which request
                  created it.
                </p>
              )}
              <p>
                Check its run history for delivery outcomes. Saving
                configuration does not prove a successful connection or
                delivery.
              </p>
              {error && (
                <p role="status" className="destination-error">
                  {error}
                </p>
              )}
            </div>
          ) : null}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={sending}
              onClick={() => void close()}
            >
              {step === "receipt" ? "Close receipt" : "Cancel"}
            </Button>
            {step === "configure" && (
              <Button
                type="submit"
                form={`${id}-form`}
                disabled={
                  working ||
                  !canWrite ||
                  !catalog.data?.length ||
                  catalog.isError
                }
              >
                {phase === "loading" ? "Loading review…" : "Review destination"}
              </Button>
            )}
            {step === "review" && (
              <>
                <Button variant="outline" disabled={working} onClick={edit}>
                  Edit configuration
                </Button>
                <Button
                  disabled={
                    working ||
                    !canWrite ||
                    needsReview ||
                    uncertain ||
                    (!!reviewed?.enabled && !enabledConfirmed) ||
                    (!!reviewed?.destination_type.endsWith("_ads") &&
                      !consentConfirmed)
                  }
                  onClick={() => void submit()}
                >
                  {sending ? "Creating…" : "Create destination"}
                </Button>
              </>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
