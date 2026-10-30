import { useRef, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { useLeaveGuard } from "@/features/navigation/leave-guard-provider"
import {
  Building2,
  Users,
  Pencil,
  Check,
  ArrowUpRight,
  Link2,
  ShieldCheck,
  RefreshCw
} from "lucide-react"
import {
  Button,
  Input,
  Textarea,
  Field,
  IconButton,
  NativeSelect,
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  Modal as Dialog,
  ModalContent as DialogContent,
  ModalHeader as DialogHeader,
  ModalTitle as DialogTitle,
  ModalDescription as DialogDescription,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent
} from "@/design-system/primitives"
import {
  createRecordDraft,
  isRecordDraftDirty,
  recordDraftWrite,
  addDraftField,
  currentTitle,
  fieldType,
  DraftFieldError
} from "@/features/records/record-draft"
import { RecordConflictReview } from "@/features/records/conflict-review"
import { CustomFieldCreator } from "@/features/records/custom-field-creator"
import { CompanyPicker } from "@/features/records/company-picker"
import { CompanyPeople } from "@/features/records/company-people"
import { StatusBadge, ErrorState } from "@/design-system/patterns/workbench"
import { getActiveWorkspace } from "@/lib/auth"
import { relativeTime, recordTimestamp } from "@/lib/record-overview"
import {
  displayField,
  recordAPI,
  fetchSharedRecord,
  validateSharedRecord,
  RecordRequestError,
  type RecordKind,
  type SharedRecord
} from "@/lib/records"
import "./record-inspector.css"

interface Observation {
  id: number
  record_type: string
  field: string
  value: unknown
  source: string
  observed_at: string
  accepted: boolean
}
const labels: Record<string, string> = {
  name: "Company name",
  full_name: "Full name",
  domain: "Company domain",
  email: "Work email",
  phone: "Phone",
  industry: "Industry",
  city: "City",
  description: "Description",
  linkedin_url: "LinkedIn URL",
  score: "Fit score",
  score_tier: "Fit tier",
  employee_count_exact: "Employee count",
  full_address: "Address",
  revenue_range: "Revenue range",
  contact_title: "Role"
}
const fieldLabel = (key: string) =>
  labels[key] ||
  key.replaceAll("_", " ").replace(/^./, (char) => char.toUpperCase())
const sourceLabel = (value: string) =>
  value === "manual"
    ? "Manual review"
    : value === "entity_graph"
      ? "Connected record"
      : value.replace(/^(import|provider|connector):/, "").replaceAll("_", " ")

export function RecordEditor({
  kind,
  record,
  onClose,
  onSaved,
  readOnly = false,
  refreshing = false,
  refreshError,
  onRetry
}: {
  kind: RecordKind
  record?: SharedRecord
  onClose: () => void
  onSaved: () => void
  readOnly?: boolean
  refreshing?: boolean
  refreshError?: Error
  onRetry?: () => void
}) {
  const [baseline, setBaseline] = useState(record)
  const [draft, setDraft] = useState(() => createRecordDraft(kind, record))
  const [tab, setTab] = useState("overview")
  const [editing, setEditing] = useState(!record)
  const [confirmClose, setConfirmClose] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [conflict, setConflict] = useState<SharedRecord | null>(null)
  const pendingSave = useRef(false)
  const dirty = isRecordDraftDirty(draft, baseline)
  const releaseDraft = useLeaveGuard({ dirty: editing && dirty, busy })
  const workspace = getActiveWorkspace()
  const history = useQuery({
    queryKey: ["record-history", workspace, kind, record?.id, record?.version],
    queryFn: ({ signal }) =>
      recordAPI<Observation[]>(
        `records/${kind}/${record!.id}/history`,
        undefined,
        "GET",
        signal
      ),
    enabled: !!record && (tab === "sources" || tab === "history")
  })
  const shown = editing ? baseline : record
  const oldTitle = currentTitle(shown)
  const requestClose = () => {
    if (pendingSave.current) return
    if (dirty && editing) setConfirmClose(true)
    else {
      releaseDraft()
      onClose()
    }
  }
  const keepEditing = () => {
    setConfirmClose(false)
  }
  const discard = () => {
    if (pendingSave.current) return
    releaseDraft()
    setConfirmClose(false)
    onClose()
  }
  function startEditing() {
    setBaseline(record)
    setDraft(createRecordDraft(kind, record))
    setEditing(true)
    setError("")
    setFieldErrors({})
  }
  function changeField(key: string, value: string) {
    setDraft((previous) => ({
      ...previous,
      fields: { ...previous.fields, [key]: value }
    }))
    setFieldErrors((previous) =>
      Object.fromEntries(
        Object.entries(previous).filter(([field]) => field !== key)
      )
    )
    setError("")
  }
  const Icon = kind === "company" ? Building2 : Users
  const name =
    displayField(
      shown?.fields.name ||
        shown?.fields.full_name ||
        shown?.fields.domain ||
        shown?.fields.email
    ) || (record ? `Unnamed ${kind}` : `New ${kind}`)
  async function reviewCurrent() {
    if (!baseline) return
    recordDraftWrite(kind, draft, baseline)
    const latest = await fetchSharedRecord(kind, baseline.id)
    if (latest.version < baseline.version)
      throw new Error(
        "The current response is older than the reviewed draft. Your draft is kept; load the current version again."
      )
    if (latest.version === baseline.version) {
      setError(
        "The shared version has not changed. Review email, profile, domain and source identifiers before retrying. Your draft is kept."
      )
      return
    }
    setConflict(latest)
    setTab("overview")
    setError("")
  }
  async function save() {
    if (pendingSave.current || conflict) return
    if (readOnly) {
      setError("You have view access. Your draft is kept.")
      return
    }
    let body: ReturnType<typeof recordDraftWrite>
    try {
      body = recordDraftWrite(kind, draft, baseline)
    } catch (cause) {
      if (cause instanceof DraftFieldError) {
        setFieldErrors({ [cause.field]: cause.message })
        setTab("overview")
        requestAnimationFrame(() =>
          document
            .getElementById(
              cause.field === "$employment"
                ? "record-current-company"
                : cause.field === "$custom"
                  ? "record-custom-name"
                  : `record-field-${encodeURIComponent(cause.field)}`
            )
            ?.focus()
        )
      }
      setError(
        cause instanceof Error ? cause.message : "Check the draft values."
      )
      return
    }
    if (baseline && record && record.version > baseline.version) {
      setConflict(record)
      setTab("overview")
      return
    }
    pendingSave.current = true
    setBusy(true)
    setError("")
    setFieldErrors({})
    try {
      const result = await recordAPI<unknown>(
        `records/${kind}${baseline ? `/${baseline.id}` : ""}`,
        baseline ? { ...body, version: baseline.version } : body,
        baseline ? "PATCH" : "POST"
      )
      const id =
        baseline?.id ||
        (result &&
        typeof result === "object" &&
        "id" in result &&
        typeof result.id === "string"
          ? result.id
          : "")
      validateSharedRecord(result, kind, id)
      releaseDraft()
      onSaved()
      onClose()
    } catch (cause) {
      if (
        cause instanceof RecordRequestError &&
        cause.status === 409 &&
        baseline
      ) {
        try {
          await reviewCurrent()
        } catch {
          setError(
            "The record conflicted, and the current version could not be loaded. Your draft is kept. Try reviewing the current version again."
          )
        }
      } else
        setError(
          cause instanceof Error
            ? cause.message
            : "Could not confirm the save. Your draft is kept."
        )
    } finally {
      pendingSave.current = false
      setBusy(false)
    }
  }
  function useValue(observation: Observation) {
    const next = editing ? draft : createRecordDraft(kind, record)
    if (!editing) setBaseline(record)
    setDraft({
      ...next,
      fields: {
        ...next.fields,
        [observation.field]: displayField(observation.value)
      },
      types: {
        ...next.types,
        [observation.field]: fieldType(observation.value)
      },
      added: Object.hasOwn(next.fields, observation.field)
        ? next.added
        : [...next.added, observation.field]
    })
    setEditing(true)
    setConflict(null)
    setTab("overview")
    setError("")
  }
  return (
    <>
      <Sheet
        open
        onOpenChange={(open) => {
          if (!open) requestClose()
        }}
      >
        <SheetContent className="record-inspector">
          <SheetHeader className="record-inspector-header">
            <div className="record-inspector-kind">
              <Icon aria-hidden="true" />
              <span>
                {kind === "company" ? "Company record" : "Person record"}
              </span>
            </div>
            <SheetTitle>{name}</SheetTitle>
            <SheetDescription>
              {kind === "company"
                ? displayField(record?.fields.domain) ||
                  "Shared company facts and history"
                : displayField(record?.company?.name) ||
                  "Independent person, connected history"}
            </SheetDescription>
            {readOnly && (
              <p className="record-access-note" role="status">
                View only. Editing shared facts requires an editor or admin
                role.{editing && dirty ? " Your draft is kept." : ""}
              </p>
            )}
            {record && (
              <div className="record-inspector-meta">
                <StatusBadge tone="positive">
                  <ShieldCheck aria-hidden="true" className="size-3" />
                  Shared record
                </StatusBadge>
                <span>
                  {editing ? "Editing version" : "Version"} {shown?.version}
                </span>
                {onRetry && (
                  <IconButton
                    className="record-refresh"
                    label="Refresh record"
                    title="Refresh record"
                    size="icon-sm"
                    variant="ghost"
                    disabled={refreshing || busy}
                    aria-busy={refreshing || undefined}
                    onClick={onRetry}
                  >
                    <RefreshCw aria-hidden="true" className="size-3.5" />
                  </IconButton>
                )}
              </div>
            )}
            {editing &&
              baseline &&
              record &&
              record.version > baseline.version && (
                <p role="status" className="text-xs text-muted-foreground">
                  A newer shared version is available. Saving will open a
                  review; your draft is kept.
                </p>
              )}
            {refreshing && (
              <p role="status" className="text-xs text-muted-foreground">
                Checking current record… Your draft is kept.
              </p>
            )}
            {refreshError && (
              <p role="alert" className="text-xs text-destructive">
                Refresh failed. Showing the last loaded version; your draft is
                kept.{" "}
                <button type="button" onClick={onRetry}>
                  Try again
                </button>
              </p>
            )}
          </SheetHeader>
          <Tabs
            value={tab}
            onValueChange={(value) => setTab(String(value))}
            className="record-inspector-tabs"
          >
            <TabsList
              variant="line"
              aria-label="Record views"
              className="record-inspector-tablist"
            >
              <TabsTrigger value="overview">Overview</TabsTrigger>
              {kind === "company" && record && (
                <TabsTrigger value="people">People</TabsTrigger>
              )}
              {kind === "person" && record && (
                <TabsTrigger value="employment">Employment</TabsTrigger>
              )}
              {record && (
                <>
                  <TabsTrigger value="sources">Sources</TabsTrigger>
                  <TabsTrigger value="history">Activity</TabsTrigger>
                </>
              )}
            </TabsList>
            <div className="record-inspector-scroll">
              <TabsContent value="overview">
                {conflict && baseline ? (
                  <RecordConflictReview
                    key={conflict.version}
                    kind={kind}
                    baseline={baseline}
                    draft={draft}
                    latest={conflict}
                    label={fieldLabel}
                    onResolved={(resolved) => {
                      setBaseline(conflict)
                      setDraft(resolved)
                      setConflict(null)
                      setError("")
                      setFieldErrors({})
                    }}
                    onCancel={() => setConflict(null)}
                  />
                ) : !editing && record ? (
                  <>
                    <div className="record-inspector-section-heading">
                      <h2>Accepted facts</h2>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={readOnly}
                        onClick={startEditing}
                      >
                        <Pencil />
                        Edit record
                      </Button>
                    </div>
                    <dl className="record-facts">
                      {Object.entries(record.fields).map(([key, value]) => (
                        <div className="record-fact" key={key}>
                          <dt>{fieldLabel(key)}</dt>
                          <dd>
                            <span className="record-fact-value">
                              {displayField(value) || (
                                <span className="record-fact-empty">
                                  Not provided
                                </span>
                              )}
                            </span>
                            {record.field_owners?.[key] && (
                              <span className="record-fact-origin">
                                {record.field_owners[key].source ===
                                  "manual" && (
                                  <ShieldCheck aria-hidden="true" />
                                )}
                                {sourceLabel(record.field_owners[key].source)}
                                {record.field_owners[key].source === "manual"
                                  ? " · protected"
                                  : ""}
                              </span>
                            )}
                          </dd>
                        </div>
                      ))}
                    </dl>
                    {kind === "person" && (
                      <div className="record-current-employment">
                        <h3>Current employment</h3>
                        <strong>
                          {displayField(record.company?.name) ||
                            "Company unknown"}
                        </strong>
                        <p>{oldTitle || "Role not provided"}</p>
                        {record.company_id && (
                          <Link
                            to={`/companies?record=${encodeURIComponent(record.company_id)}`}
                          >
                            View company
                            <ArrowUpRight aria-hidden="true" />
                          </Link>
                        )}
                      </div>
                    )}
                    <div className="record-shared-explainer">
                      <Link2 aria-hidden="true" />
                      <p>
                        Accepted changes appear in every linked workbook and
                        audience. Source observations remain in your history.
                      </p>
                    </div>
                  </>
                ) : (
                  <form
                    id="record-edit-form"
                    className="record-edit-form"
                    onSubmit={(event) => {
                      event.preventDefault()
                      void save()
                    }}
                  >
                    <div className="record-inspector-section-heading">
                      <h2>
                        {record
                          ? "Edit shared facts"
                          : "Create a shared record"}
                      </h2>
                    </div>
                    {Object.entries(draft.fields).map(([key, value]) => {
                      const type = draft.types[key]
                      return (
                        <Field
                          key={key}
                          id={`record-field-${encodeURIComponent(key)}`}
                          label={fieldLabel(key)}
                          error={fieldErrors[key]}
                          className="record-edit-field"
                          description={
                            type === "json"
                              ? "JSON value; leave empty to clear."
                              : type === "number"
                                ? "Number; leave empty to clear. Store long identifiers as text."
                                : undefined
                          }
                        >
                          {(control) =>
                            type === "boolean" ? (
                              <NativeSelect
                                {...control}
                                value={value}
                                disabled={busy || readOnly}
                                onChange={(event) =>
                                  changeField(key, event.target.value)
                                }
                              >
                                <option value="">Not provided</option>
                                <option value="true">Yes</option>
                                <option value="false">No</option>
                              </NativeSelect>
                            ) : type === "json" ||
                              key === "description" ||
                              value.length > 120 ? (
                              <Textarea
                                {...control}
                                value={value}
                                disabled={busy || readOnly}
                                spellCheck={type !== "json"}
                                onChange={(event) =>
                                  changeField(key, event.target.value)
                                }
                              />
                            ) : (
                              <Input
                                {...control}
                                value={value}
                                inputMode={
                                  type === "number" ? "decimal" : undefined
                                }
                                disabled={busy || readOnly}
                                onChange={(event) =>
                                  changeField(key, event.target.value)
                                }
                              />
                            )
                          }
                        </Field>
                      )
                    })}
                    <CustomFieldCreator
                      disabled={busy || readOnly}
                      pending={draft.pendingField}
                      onPendingChange={(pendingField) => {
                        setDraft((previous) => ({ ...previous, pendingField }))
                        setError("")
                        setFieldErrors((previous) =>
                          Object.fromEntries(
                            Object.entries(previous).filter(
                              ([key]) => key !== "$custom"
                            )
                          )
                        )
                      }}
                      onAdd={(name, type) =>
                        setDraft(addDraftField(draft, name, type))
                      }
                    />
                    {kind === "person" && (
                      <div className="record-employment-form">
                        <h3>Employment relationship</h3>
                        <CompanyPicker
                          value={draft.companyId}
                          name={draft.companyName}
                          knownEmployer={!!baseline?.company_id}
                          disabled={busy || readOnly || draft.endEmployment}
                          onChange={(companyId, companyName) =>
                            setDraft((previous) => ({
                              ...previous,
                              companyId,
                              companyName
                            }))
                          }
                        />
                        <Field label="Current role">
                          {(control) => (
                            <Input
                              {...control}
                              value={draft.title}
                              disabled={busy || readOnly || draft.endEmployment}
                              onChange={(event) =>
                                setDraft((previous) => ({
                                  ...previous,
                                  title: event.target.value
                                }))
                              }
                            />
                          )}
                        </Field>
                        {(baseline?.company_id ||
                          baseline?.employments?.some(
                            (item) => item.current
                          )) && (
                          <label className="record-end-employment">
                            <input
                              type="checkbox"
                              checked={draft.endEmployment}
                              disabled={busy || readOnly}
                              onChange={(event) =>
                                setDraft((previous) => ({
                                  ...previous,
                                  endEmployment: event.target.checked
                                }))
                              }
                            />
                            End current employment when saving
                          </label>
                        )}
                        {draft.endEmployment ? (
                          <p role="status">
                            Saving ends the current employment. The person and
                            previous history remain. Uncheck this option to keep
                            editing the employer or role.
                          </p>
                        ) : (
                          <p>
                            Changing employer keeps this person’s identity and
                            previous employment. A role can be stored before the
                            employer is known.
                          </p>
                        )}
                        {fieldErrors.$employment && (
                          <p role="alert">{fieldErrors.$employment}</p>
                        )}
                      </div>
                    )}
                  </form>
                )}
              </TabsContent>
              <TabsContent value="people">
                {tab === "people" && kind === "company" && record && (
                  <CompanyPeople companyId={record.id} />
                )}
              </TabsContent>
              <TabsContent value="employment">
                <div className="record-inspector-section-heading">
                  <h2>Employment history</h2>
                </div>
                <div className="record-timeline">
                  {record?.employments?.map((item) => (
                    <article key={item.id} className="record-employment-entry">
                      <span className="record-rail-dot" />
                      <div>
                        <div className="record-observation-heading">
                          <strong>
                            {item.company_name || "Employer unknown"}
                          </strong>
                          <StatusBadge
                            tone={item.current ? "positive" : "neutral"}
                          >
                            {item.current ? "Current" : "Previous"}
                          </StatusBadge>
                        </div>
                        <p>
                          {displayField(item.fields.title) ||
                            "Role not provided"}
                        </p>
                        {Object.entries(item.fields)
                          .filter(([key]) => key !== "title")
                          .map(([key, value]) => (
                            <p key={key}>
                              {fieldLabel(key)}: {displayField(value)}
                            </p>
                          ))}
                        <time>
                          {new Date(
                            recordTimestamp(item.first_observed_at)
                          ).toLocaleDateString(undefined, {
                            month: "short",
                            year: "numeric"
                          })}
                        </time>
                      </div>
                    </article>
                  ))}
                </div>
                {!record?.employments?.length && (
                  <p className="record-inspector-empty">
                    No employment history yet. Add a company or role to start
                    the history.
                  </p>
                )}
              </TabsContent>
              <TabsContent value="sources">
                <div className="record-inspector-section-heading">
                  <h2>Where your facts come from</h2>
                </div>
                <p className="record-tab-description">
                  Review a source observation before making it an accepted
                  value.
                </p>
                {history.isPending ? (
                  <p role="status">Loading field sources…</p>
                ) : history.isError ? (
                  <ErrorState
                    error={history.error}
                    retry={() => void history.refetch()}
                  />
                ) : (
                  <div className="record-timeline">
                    {history.data?.map((observation) => (
                      <article
                        key={observation.id}
                        className="record-observation"
                      >
                        <span
                          className="record-rail-dot"
                          data-accepted={observation.accepted}
                        />
                        <div>
                          <div className="record-observation-heading">
                            <strong>{fieldLabel(observation.field)}</strong>
                            <StatusBadge
                              tone={
                                observation.accepted ? "positive" : "neutral"
                              }
                            >
                              {observation.accepted
                                ? "Accepted then"
                                : "Observed"}
                            </StatusBadge>
                          </div>
                          <p className="record-observation-value">
                            {displayField(observation.value)}
                          </p>
                          <div className="record-observation-source">
                            <span>
                              {sourceLabel(observation.source)} ·{" "}
                              {relativeTime(observation.observed_at)}
                            </span>
                            {observation.record_type === kind &&
                              observation.field !== "current_employment" && (
                                <Button
                                  size="sm"
                                  variant="ghost"
                                  disabled={readOnly || busy}
                                  onClick={() => useValue(observation)}
                                >
                                  Review value
                                </Button>
                              )}
                          </div>
                        </div>
                      </article>
                    ))}
                  </div>
                )}
                {!history.isPending &&
                  !history.isError &&
                  !history.data?.length && (
                    <p className="record-inspector-empty">
                      No field observations are available yet.
                    </p>
                  )}
                {!!record?.external_ids?.length && (
                  <section className="record-source-identities">
                    <h3>Connected source rows</h3>
                    {record.external_ids.map((identity) => (
                      <div key={`${identity.source}:${identity.external_id}`}>
                        <strong>{sourceLabel(identity.source)}</strong>
                        <code>{identity.external_id}</code>
                      </div>
                    ))}
                  </section>
                )}
              </TabsContent>
              <TabsContent value="history">
                <div className="record-inspector-section-heading">
                  <h2>Record activity</h2>
                </div>
                {history.isPending ? (
                  <p role="status">Loading activity…</p>
                ) : history.isError ? (
                  <ErrorState
                    error={history.error}
                    retry={() => void history.refetch()}
                  />
                ) : (
                  <div className="record-timeline">
                    {history.data
                      ?.filter((item) => item.accepted)
                      .map((item) => (
                        <article className="record-activity" key={item.id}>
                          <span className="record-rail-dot" />
                          <div>
                            <strong>{fieldLabel(item.field)} updated</strong>
                            <p>{sourceLabel(item.source)}</p>
                            <time>{relativeTime(item.observed_at)}</time>
                          </div>
                        </article>
                      ))}
                  </div>
                )}
              </TabsContent>
            </div>
          </Tabs>
          <div className="record-inspector-footer">
            {error && (
              <div className="record-save-error">
                <p role="alert">{error}</p>
                {baseline && Object.keys(fieldErrors).length === 0 && (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={async () => {
                      setBusy(true)
                      try {
                        await reviewCurrent()
                      } catch {
                        setError(
                          "The current version could not be loaded. Your draft is kept; try again."
                        )
                      } finally {
                        setBusy(false)
                      }
                    }}
                  >
                    Review current version
                  </Button>
                )}
              </div>
            )}
            {editing ? (
              <>
                <span>
                  {dirty ? "Unsaved changes" : "Shared across your workspace"}
                </span>
                <div>
                  <Button
                    variant="outline"
                    disabled={busy}
                    onClick={requestClose}
                  >
                    Cancel
                  </Button>
                  <Button
                    type="button"
                    variant="solid"
                    loading={busy}
                    onClick={() => void save()}
                    disabled={
                      readOnly || busy || !!conflict || (!!record && !dirty)
                    }
                  >
                    <Check />
                    Save shared record
                  </Button>
                </div>
              </>
            ) : (
              <>
                <span>One record. Every workbook.</span>
                <Button variant="outline" onClick={onClose}>
                  Close record
                </Button>
              </>
            )}
          </div>
        </SheetContent>
      </Sheet>
      <Dialog
        open={confirmClose}
        onOpenChange={(open) => {
          if (!open) keepEditing()
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {pendingSave.current
                ? "Save in progress"
                : "Discard your changes?"}
            </DialogTitle>
            <DialogDescription>
              {pendingSave.current
                ? "The save is still in progress. Stay here until its result is confirmed; changes cannot be discarded while saving."
                : "Your changes have not been saved to the shared record."}
            </DialogDescription>
          </DialogHeader>
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={keepEditing}>
              Keep editing
            </Button>
            <Button
              variant="solid"
              color="danger"
              disabled={busy}
              onClick={discard}
            >
              Discard changes
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
