import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import {
  Building2,
  Users,
  Pencil,
  Check,
  ArrowUpRight,
  Link2,
  Loader2,
  Plus,
  ShieldCheck
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription
} from "@/components/ui/sheet"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription
} from "@/components/ui/dialog"
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import { StatusBadge, ErrorState } from "@/design-system/patterns/workbench"
import { getActiveWorkspace } from "@/lib/auth"
import { relativeTime, recordTimestamp } from "@/lib/record-overview"
import {
  displayField,
  recordAPI,
  type RecordKind,
  type SharedRecord,
  type RecordPage
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
  readOnly = false
}: {
  kind: RecordKind
  record?: SharedRecord
  onClose: () => void
  onSaved: () => void
  readOnly?: boolean
}) {
  const [fields, setFields] = useState<Record<string, string>>(() =>
    Object.fromEntries(
      Object.entries({
        ...(kind === "company"
          ? { name: "", domain: "", industry: "", city: "", description: "" }
          : { full_name: "", email: "", linkedin_url: "", phone: "" }),
        ...record?.fields
      }).map(([key, value]) => [key, displayField(value)])
    )
  )
  const [companyID, setCompanyID] = useState(record?.company_id ?? "")
  const oldTitle = displayField(
    record?.employments?.find((item) => item.current)?.fields.title
  )
  const [title, setTitle] = useState(oldTitle)
  const [tab, setTab] = useState("overview")
  const [editing, setEditing] = useState(!record)
  const [confirmClose, setConfirmClose] = useState(false)
  const [custom, setCustom] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const workspace = getActiveWorkspace()
  const history = useQuery({
    queryKey: ["record-history", workspace, kind, record?.id],
    queryFn: ({ signal }) =>
      recordAPI<Observation[]>(
        `records/${kind}/${record!.id}/history`,
        undefined,
        "GET",
        signal
      ),
    enabled: !!record && (tab === "sources" || tab === "history")
  })
  const companies = useQuery({
    queryKey: ["record-companies-picker", workspace],
    queryFn: ({ signal }) =>
      recordAPI<RecordPage>(
        "records/company?limit=500",
        undefined,
        "GET",
        signal
      ),
    enabled: kind === "person" && editing
  })
  const dirty =
    Object.entries(fields).some(
      ([key, value]) => value !== displayField(record?.fields[key])
    ) ||
    companyID !== (record?.company_id ?? "") ||
    title !== oldTitle
  const requestClose = () => {
    if (busy) return
    if (dirty && editing) setConfirmClose(true)
    else onClose()
  }
  const Icon = kind === "company" ? Building2 : Users
  const name =
    displayField(record?.fields.name || record?.fields.full_name) ||
    `New ${kind}`
  async function save() {
    if (readOnly) {
      setError("You have view access. Your draft is kept.")
      return
    }
    setBusy(true)
    setError("")
    try {
      const data: Record<string, unknown> = {}
      for (const [key, value] of Object.entries(fields)) {
        const old = record?.fields[key]
        if (record && value === displayField(old)) continue
        if (typeof old === "number") {
          const number = value === "" ? null : Number(value)
          if (number !== null && !Number.isFinite(number))
            throw new Error(`${fieldLabel(key)} must be a number.`)
          data[key] = number
        } else if (typeof old === "boolean") data[key] = value === "true"
        else if (typeof old === "object" && old !== null) {
          try {
            data[key] = JSON.parse(value)
          } catch {
            throw new Error(
              `${fieldLabel(key)} must contain valid JSON. Your draft is kept.`
            )
          }
        } else data[key] = value
      }
      const relation =
        companyID !== (record?.company_id ?? "")
          ? companyID
            ? { company_id: companyID }
            : { end_employment: true }
          : {}
      const body =
        kind === "company"
          ? { company: data }
          : {
              person: data,
              ...relation,
              ...(title !== oldTitle ? { employment: { title } } : {})
            }
      await recordAPI(
        `records/${kind}${record ? `/${record.id}` : ""}`,
        record ? { ...body, version: record.version } : body,
        record ? "PATCH" : "POST"
      )
      onSaved()
      onClose()
    } catch (cause) {
      setError(
        cause instanceof Error
          ? cause.message
          : "Could not save record. Your draft is kept."
      )
    } finally {
      setBusy(false)
    }
  }
  function useValue(observation: Observation) {
    setFields((previous) => ({
      ...previous,
      [observation.field]: displayField(observation.value)
    }))
    setEditing(true)
    setTab("overview")
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
            {record && (
              <div className="record-inspector-meta">
                <StatusBadge tone="positive">
                  <ShieldCheck aria-hidden="true" className="size-3" />
                  Shared record
                </StatusBadge>
                <span>Version {record.version}</span>
              </div>
            )}
          </SheetHeader>
          <Tabs
            value={tab}
            onValueChange={(value) => setTab(String(value))}
            className="record-inspector-tabs"
          >
            <TabsList variant="line" className="record-inspector-tablist">
              <TabsTrigger value="overview">Overview</TabsTrigger>
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
                {!editing && record ? (
                  <>
                    <div className="record-inspector-section-heading">
                      <h2>Accepted facts</h2>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={readOnly}
                        onClick={() => setEditing(true)}
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
                            to={`/companies?q=${encodeURIComponent(displayField(record.company?.domain || record.company?.name))}`}
                            onClick={onClose}
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
                    {Object.entries(fields).map(([key, value]) => (
                      <label key={key} className="record-edit-field">
                        <span>{fieldLabel(key)}</span>
                        {typeof record?.fields[key] === "boolean" ? (
                          <select
                            value={value}
                            onChange={(event) =>
                              setFields({
                                ...fields,
                                [key]: event.target.value
                              })
                            }
                            disabled={busy}
                          >
                            <option value="true">Yes</option>
                            <option value="false">No</option>
                          </select>
                        ) : key === "description" ||
                          value.length > 120 ||
                          (typeof record?.fields[key] === "object" &&
                            record?.fields[key] !== null) ? (
                          <Textarea
                            value={value}
                            disabled={busy}
                            onChange={(event) =>
                              setFields({
                                ...fields,
                                [key]: event.target.value
                              })
                            }
                          />
                        ) : (
                          <Input
                            value={value}
                            disabled={busy}
                            onChange={(event) =>
                              setFields({
                                ...fields,
                                [key]: event.target.value
                              })
                            }
                          />
                        )}
                      </label>
                    ))}
                    <div className="record-custom-field">
                      <Input
                        aria-label="Custom field name"
                        placeholder="Custom field name"
                        value={custom}
                        onChange={(event) => setCustom(event.target.value)}
                      />
                      <Button
                        type="button"
                        variant="outline"
                        size="icon"
                        aria-label="Add custom field"
                        disabled={!custom.trim() || custom.trim() in fields}
                        onClick={() => {
                          setFields({ ...fields, [custom.trim()]: "" })
                          setCustom("")
                        }}
                      >
                        <Plus />
                      </Button>
                    </div>
                    {kind === "person" && (
                      <div className="record-employment-form">
                        <h3>Employment relationship</h3>
                        <label className="record-edit-field">
                          <span>Current company</span>
                          <select
                            value={companyID}
                            onChange={(event) =>
                              setCompanyID(event.target.value)
                            }
                            disabled={busy}
                          >
                            <option value="">
                              Company unknown / end current employment
                            </option>
                            {record?.company_id &&
                              !companies.data?.records.some(
                                (company) => company.id === record.company_id
                              ) && (
                                <option value={record.company_id}>
                                  {displayField(record.company?.name) ||
                                    "Current company"}
                                </option>
                              )}
                            {companies.data?.records.map((company) => (
                              <option key={company.id} value={company.id}>
                                {displayField(company.fields.name)} ·{" "}
                                {displayField(company.fields.domain)}
                              </option>
                            ))}
                          </select>
                        </label>
                        {companies.isError && (
                          <p role="alert">
                            Could not load companies.{" "}
                            <button
                              type="button"
                              onClick={() => void companies.refetch()}
                            >
                              Try again
                            </button>
                          </p>
                        )}
                        <label className="record-edit-field">
                          <span>Current role</span>
                          <Input
                            value={title}
                            disabled={busy}
                            onChange={(event) => setTitle(event.target.value)}
                          />
                        </label>
                        <p>
                          Changing employer keeps this person's identity and
                          previous employment. A role can be stored before the
                          employer is known.
                        </p>
                      </div>
                    )}
                  </form>
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
                                  disabled={readOnly}
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
                {!history.isPending && !history.data?.length && (
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
            {error && <p role="alert">{error} Your draft is kept.</p>}
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
                    onClick={() => void save()}
                    disabled={readOnly || busy || (!!record && !dirty)}
                  >
                    {busy ? <Loader2 className="animate-spin" /> : <Check />}
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
      <Dialog open={confirmClose} onOpenChange={setConfirmClose}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Discard your changes?</DialogTitle>
            <DialogDescription>
              Your changes have not been saved to the shared record.
            </DialogDescription>
          </DialogHeader>
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={() => setConfirmClose(false)}>
              Keep editing
            </Button>
            <Button variant="destructive" onClick={onClose}>
              Discard changes
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
