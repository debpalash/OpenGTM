import { useUrlSearchInput } from "@/lib/use-url-search-input"
import { useDeferredValue, useState, useRef } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useNavigate, useSearchParams } from "react-router-dom"
import {
  Upload,
  Download,
  Plus,
  Sparkles,
  Settings2,
  Search,
  ArrowUpRight,
  Loader2,
  ListFilter,
  MoreHorizontal,
  ChevronDown,
  CheckCircle2,
  ArrowRight,
  SlidersHorizontal
} from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { RecordImportDialog } from "@/components/records/import-dialog"
import { RecordEditor } from "@/components/records/record-editor"
import { FindPeopleDialog } from "@/components/records/find-people-dialog"
import { SnapshotDialog } from "@/components/records/snapshot-dialog"
import { EnrollPeopleDialog } from "@/components/records/enroll-dialog"
import { WorkflowDialog } from "@/components/records/workflow-dialog"
import {
  PageHeader,
  ErrorState,
  ViewState,
  StatusBadge
} from "@/design-system/patterns/workbench"
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator
} from "@/components/ui/dropdown-menu"
import { relativeTime } from "@/lib/record-overview"
import "./records.css"
import { getActiveWorkspace } from "@/lib/auth"
import {
  displayField,
  downloadData,
  recordAPI,
  type RecordKind,
  type RecordPage,
  type SharedRecord,
  type RecordWorkflow
} from "@/lib/records"

export default function RecordsPage({ kind }: { kind: RecordKind }) {
  const navigate = useNavigate()
  const client = useQueryClient()
  const workspace = getActiveWorkspace()
  const [params, setParams] = useSearchParams()
  const search = params.get("q") || ""
  const searchInput = useUrlSearchInput(search,value=>setView("q",value))
  const deferred = useDeferredValue(searchInput.value)
  const offsetValue = Number(params.get("offset"))
  const offset =
    Number.isSafeInteger(offsetValue) && offsetValue >= 0 ? offsetValue : 0
  const setOffset = (value: number) =>
    setParams((previous) => {
      const next = new URLSearchParams(previous)
      if (value) next.set("offset", String(value))
      else next.delete("offset")
      return next
    })
  const filter = [
    "all",
    "qualified",
    kind === "person" ? "needs_email" : "needs_domain"
  ].includes(params.get("filter") || "all")
    ? params.get("filter") || "all"
    : "all"
  const sort = ["name", "updated", "score"].includes(
    params.get("sort") || "name"
  )
    ? params.get("sort") || "name"
    : "name"
  const [density, setDensity] = useState("default")
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const returnFocus = useRef<HTMLElement | null>(null)
  const setView = (key: string, value: string) => {
    setSelected(new Set())
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous)
        if (value && value !== "all") next.set(key, value)
        else next.delete(key)
        next.delete("offset")
        return next
      },
      { replace: true }
    )
  }
  const updateSearch = (value: string) => {
    searchInput.onChange(value)
  }
  const [discoveryOpen, setDiscoveryOpen] = useState(false)
  const [snapshotOpen, setSnapshotOpen] = useState(false)
  const [enrollOpen, setEnrollOpen] = useState(false)
  const [importOpen, setImportOpen] = useState(params.get("import") === "1")
  const [editor, setEditor] = useState<SharedRecord | "new" | null>(null)
  const [workflowOpen, setWorkflowOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const plural = kind === "company" ? "Companies" : "People"
  const query = useQuery({
    queryKey: ["records", workspace, kind, deferred, offset, filter, sort],
    queryFn: ({ signal }) =>
      recordAPI<RecordPage>(
        `records/${kind}?limit=100&offset=${offset}&search=${encodeURIComponent(deferred)}&filter=${filter}&sort=${sort}&direction=${sort === "name" ? "asc" : "desc"}`,
        undefined,
        "GET",
        signal
      ),
    refetchInterval: 20_000
  })
  const access = useQuery({
    queryKey: ["record-access", workspace],
    queryFn: ({ signal }) =>
      recordAPI<{ can_write: boolean }>(
        "records/access",
        undefined,
        "GET",
        signal
      ),
    staleTime: 30000
  })
  const canWrite = access.data?.can_write === true
  const workflow = useQuery({
    queryKey: ["record-workflow", workspace, kind],
    queryFn: ({ signal }) =>
      recordAPI<RecordWorkflow>(
        `records/${kind}/workflow`,
        undefined,
        "GET",
        signal
      )
  })
  const rows = query.data?.records ?? []
  const changed = () => {
    void client.invalidateQueries({ queryKey: ["records"] })
    void client.invalidateQueries({ queryKey: ["record-companies-picker"] })
    void client.invalidateQueries({ queryKey: ["record-overview"] })
  }
  async function edit(r: SharedRecord) {
    returnFocus.current = document.activeElement as HTMLElement
    try {
      setEditor(await recordAPI<SharedRecord>(`records/${kind}/${r.id}`))
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Could not open record")
    }
  }
  async function workbook(run: boolean) {
    setBusy(true)
    try {
      const out = await recordAPI<{ workbook_id: string }>(
        `records/${kind}/workbook`,
        { ids: [...selected] }
      )
      navigate(
        `/workbooks/${out.workbook_id}${run && workflow.data?.columns.length ? "?review=1" : ""}`
      )
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Could not create workbook")
    } finally {
      setBusy(false)
    }
  }
  async function audience() {
    setBusy(true)
    try {
      const out = await recordAPI<{ audience_id: string }>(
        `records/${kind}/audience`,
        {
          ids: [...selected],
          name: `${plural} · ${new Date().toLocaleDateString()}`
        }
      )
      await recordAPI(`audiences/${out.audience_id}/refresh`, {})
      navigate("/audiences")
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Could not create audience")
    } finally {
      setBusy(false)
    }
  }
  async function exportCSV() {
    setBusy(true)
    try {
      const res = await fetch(`/api/records/${kind}/export`)
      if (!res.ok) throw new Error("Could not export records")
      downloadData(
        await res.text(),
        `opengtm-${kind}.csv`,
        "text/csv;charset=utf-8"
      )
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Export failed")
    } finally {
      setBusy(false)
    }
  }
  async function backup() {
    setBusy(true)
    try {
      const res = await fetch("/api/record-snapshots")
      if (!res.ok) throw new Error("Could not export backup")
      downloadData(await res.text(), "opengtm-records.json")
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Backup failed")
    } finally {
      setBusy(false)
    }
  }
  async function monitor() {
    setBusy(true)
    try {
      for (const id of selected) {
        const r =
          rows.find((item) => item.id === id) ??
          (await recordAPI<SharedRecord>(`records/company/${id}`))
        await recordAPI("watches", {
          kind: "company",
          company_id: id,
          target: displayField(r.fields.domain || r.fields.name),
          interval: "daily"
        })
      }
      toast.success("Company watches created")
      navigate("/watches")
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Could not create watches")
    } finally {
      setBusy(false)
    }
  }
  function toggle(id: string) {
    setSelected((previous) => {
      const next = new Set(previous)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  function closeEditor() {
    setEditor(null)
    requestAnimationFrame(() => returnFocus.current?.focus())
  }
  const checkedOnPage = rows.filter((row) => selected.has(row.id)).length
  const fitLabel = (record: SharedRecord) =>
    ({
      hot: "High fit",
      warm: "Potential fit",
      cold: "Low fit",
      unqualified: "Unqualified"
    })[String(record.fields.score_tier)] || "Not scored"
  const currentRole = (record: SharedRecord) =>
    displayField(record.employments?.find((item) => item.current)?.fields.title)
  return (
    <div className="workbench-page records-page">
      <PageHeader
        title={plural}
        description={
          kind === "company"
            ? "Build and qualify your account universe."
            : "The right people. A connected history."
        }
        actions={
          <>
            <Button disabled={!canWrite} onClick={() => setImportOpen(true)}>
              <Upload aria-hidden="true" />
              <span className="records-import-label-wide">Import from Clay</span>
              <span className="records-import-label-short">Import</span>
            </Button>
            <Button
              variant="outline"
              disabled={!canWrite}
              className="records-new-button hidden sm:inline-flex"
              onClick={() => {
                returnFocus.current = document.activeElement as HTMLElement
                setEditor("new")
              }}
            >
              <Plus aria-hidden="true" />
              New {kind}
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`More ${plural.toLowerCase()} actions`}
                  />
                }
              >
                <MoreHorizontal />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="min-w-56">
                <DropdownMenuItem
                  disabled={!canWrite}
                  onClick={() => setEditor("new")}
                >
                  <Plus />
                  New {kind}
                </DropdownMenuItem>
                <DropdownMenuItem
                  disabled={busy}
                  onClick={() => void exportCSV()}
                >
                  <Download />
                  Export table CSV
                </DropdownMenuItem>
                <DropdownMenuItem disabled={busy} onClick={() => void backup()}>
                  Back up records JSON
                </DropdownMenuItem>
                <DropdownMenuItem
                  disabled={!canWrite}
                  onClick={() => setSnapshotOpen(true)}
                >
                  Restore record backup
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  disabled={!workflow.data || !canWrite}
                  onClick={() => setWorkflowOpen(true)}
                >
                  <Settings2 />
                  Enrichment workflow
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />
      <div className="workbench-toolbar records-toolbar">
        <div className="workbench-search">
          <Search aria-hidden="true" />
          <Input
            type="search"
            aria-label={`Search ${plural.toLowerCase()}`}
            placeholder={`Search ${plural.toLowerCase()}…`}
            value={searchInput.value}
            onChange={(event) => updateSearch(event.target.value)}
          />
        </div>
        <select
          aria-label={`Filter ${plural.toLowerCase()}`}
          className="records-filter"
          value={filter}
          onChange={(event) => setView("filter", event.target.value)}
        >
          <option value="all">All {plural.toLowerCase()}</option>
          <option value="qualified">High & potential fit</option>
          <option value={kind === "person" ? "needs_email" : "needs_domain"}>
            {kind === "person" ? "Needs work email" : "Needs company domain"}
          </option>
        </select>
        <span className="workbench-toolbar-meta" role="status">
          {query.data
            ? `${query.data.total.toLocaleString()} ${query.data.total === 1 ? "record" : "records"}`
            : "Loading records…"}
        </span>
        <div className="workbench-toolbar-spacer" />
        <div className="records-view-options">
          <label className="sr-only" htmlFor="records-sort">
            Sort records
          </label>
          <select
            id="records-sort"
            value={sort}
            onChange={(event) => setView("sort", event.target.value)}
          >
            <option value="name">Name A–Z</option>
            <option value="updated">Recently updated</option>
            <option value="score">Highest score</option>
          </select>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Table density"
                />
              }
            >
              <SlidersHorizontal />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={() => setDensity("compact")}>
                Compact rows
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => setDensity("default")}>
                Default rows
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => setDensity("comfortable")}>
                Comfortable rows
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      {selected.size > 0 && (
        <div
          className="records-selection"
          role="region"
          aria-label="Selected records"
        >
          <span>
            <strong>{selected.size}</strong> records selected · {checkedOnPage}{" "}
            on this page
          </span>
          <div>
            <Button
              size="sm"
              disabled={busy || !canWrite}
              onClick={() => void workbook(true)}
            >
              <Sparkles />
              {workflow.data?.columns.length
                ? "Enrich selected"
                : "Open workbook"}
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy || !canWrite}
                  />
                }
              >
                More actions
                <ChevronDown />
              </DropdownMenuTrigger>
              <DropdownMenuContent>
                <DropdownMenuItem onClick={() => void audience()}>
                  <ListFilter />
                  Create audience
                </DropdownMenuItem>
                {kind === "company" ? (
                  <>
                    <DropdownMenuItem
                      disabled={selected.size > 100}
                      onClick={() => setDiscoveryOpen(true)}
                    >
                      Find people
                    </DropdownMenuItem>
                    <DropdownMenuItem onClick={() => void monitor()}>
                      Monitor companies
                    </DropdownMenuItem>
                  </>
                ) : (
                  <DropdownMenuItem onClick={() => setEnrollOpen(true)}>
                    Enroll in outreach
                  </DropdownMenuItem>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
            <Button
              size="sm"
              variant="ghost"
              disabled={busy}
              onClick={() => setSelected(new Set())}
            >
              Clear
            </Button>
            {busy && (
              <Loader2
                className="size-4 animate-spin"
                aria-label="Working on selected records"
              />
            )}
          </div>
        </div>
      )}
      {!access.isPending && !canWrite && (
        <div role="status" className="records-permission">
          {access.isError ? (
            <>
              Could not check editing access.{" "}
              <button onClick={() => void access.refetch()}>Try again</button>
            </>
          ) : (
            "You have view access. Ask a workspace admin for permission to import or edit records."
          )}
        </div>
      )}
      <div className="records-body">
        {query.isPending ? (
          <ViewState
            kind="loading"
            title={`Loading ${plural.toLowerCase()}…`}
          />
        ) : query.isError ? (
          <ErrorState error={query.error} retry={() => void query.refetch()} />
        ) : rows.length === 0 ? (
          <ViewState
            title={
              search || filter !== "all"
                ? "No matching records"
                : `Your ${plural.toLowerCase()} start here`
            }
            description={
              search || filter !== "all"
                ? "Try another search or clear the current filter."
                : "Bring your table from Clay or a CSV. Your accepted facts stay connected across every workbook."
            }
            action={
              search || filter !== "all" ? (
                <Button
                  variant="outline"
                  onClick={() => {
                    setParams({})
                    setSelected(new Set())
                  }}
                >
                  Clear search and filters
                </Button>
              ) : (
                <Button
                  disabled={!canWrite}
                  onClick={() => setImportOpen(true)}
                >
                  <Upload />
                  Import a table
                </Button>
              )
            }
          />
        ) : (
          <div
            className="records-table-scroll"
            role="region"
            aria-label={`${plural} table, scroll for more columns`}
            tabIndex={0}
          >
            <table className="records-table" data-density={density}>
              <caption className="sr-only">
                {plural}, {query.data.total.toLocaleString()} matching records.
                Select a name to inspect shared facts and history.
              </caption>
              <thead>
                <tr>
                  <th scope="col" className="records-check">
                    <label className="gtm-checkbox-target">
                      <input
                        type="checkbox"
                        aria-label="Select all records on this page"
                        checked={
                          checkedOnPage === rows.length && rows.length > 0
                        }
                        ref={(element) => {
                          if (element)
                            element.indeterminate =
                              checkedOnPage > 0 && checkedOnPage < rows.length
                        }}
                        onChange={(event) =>
                          setSelected((previous) => {
                            const next = new Set(previous)
                            for (const record of rows) {
                              if (event.target.checked) next.add(record.id)
                              else next.delete(record.id)
                            }
                            return next
                          })
                        }
                      />
                    </label>
                  </th>
                  <th scope="col" className="records-identity">
                    {kind === "company" ? "Company" : "Person"}
                  </th>
                  <th scope="col">
                    {kind === "company" ? "Domain" : "Company & role"}
                  </th>
                  <th scope="col">
                    {kind === "company" ? "Industry" : "Work email"}
                  </th>
                  <th scope="col">Fit</th>
                  <th scope="col">
                    {kind === "company" ? "Location" : "Phone"}
                  </th>
                  <th scope="col">Updated</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((record) => (
                  <tr key={record.id} data-selected={selected.has(record.id)}>
                    <td className="records-check">
                      <label className="gtm-checkbox-target">
                        <input
                          type="checkbox"
                          aria-label={`Select ${displayField(record.fields.name || record.fields.full_name) || "unnamed record"}`}
                          checked={selected.has(record.id)}
                          onChange={() => toggle(record.id)}
                        />
                      </label>
                    </td>
                    <td className="records-identity">
                      <button
                        className="records-name"
                        onClick={() => void edit(record)}
                      >
                        <span
                          aria-hidden="true"
                          className="records-avatar"
                          data-person={kind === "person"}
                        >
                          {displayField(
                            record.fields.name || record.fields.full_name
                          )
                            .split(/\s+/)
                            .slice(0, 2)
                            .map((part) => part[0])
                            .join("")
                            .toUpperCase() || "?"}
                        </span>
                        <span>
                          {displayField(
                            record.fields.name || record.fields.full_name
                          ) || "Unnamed record"}
                        </span>
                        <ArrowUpRight aria-hidden="true" />
                      </button>
                    </td>
                    <td>
                      {kind === "company" ? (
                        <span className="records-domain">
                          {displayField(record.fields.domain) || (
                            <span className="records-missing">No domain</span>
                          )}
                        </span>
                      ) : (
                        <div className="records-two-line">
                          <Link
                            to={`/companies?q=${encodeURIComponent(displayField(record.company?.domain || record.company?.name))}`}
                          >
                            {displayField(record.company?.name) ||
                              "Company unknown"}
                          </Link>
                          {currentRole(record) && (
                            <span>{currentRole(record)}</span>
                          )}
                        </div>
                      )}
                    </td>
                    <td>
                      {kind === "company" ? (
                        <span>
                          {displayField(
                            record.fields.industry ||
                              record.fields.specialization
                          ) || <span className="records-missing">—</span>}
                        </span>
                      ) : (
                        <span className="records-email">
                          {displayField(record.fields.email) || (
                            <span className="records-missing">Not found</span>
                          )}
                          {["valid", "verified", "deliverable"].includes(
                            String(
                              record.fields.email_verify_status ||
                                record.fields.email_verify
                            ).toLowerCase()
                          ) && (
                            <CheckCircle2 aria-label="Verified work email" />
                          )}
                        </span>
                      )}
                    </td>
                    <td>
                      <StatusBadge
                        tone={
                          record.fields.score_tier === "hot"
                            ? "positive"
                            : record.fields.score_tier === "warm"
                              ? "accent"
                              : "neutral"
                        }
                      >
                        {fitLabel(record)}
                        {record.fields.score != null && (
                          <span className="records-score">
                            {displayField(record.fields.score)}
                          </span>
                        )}
                      </StatusBadge>
                    </td>
                    <td>
                      <span className="records-secondary">
                        {displayField(
                          kind === "company"
                            ? record.fields.city
                            : record.fields.phone
                        ) || "—"}
                      </span>
                    </td>
                    <td>
                      <span className="records-secondary">
                        {record.updated_at
                          ? relativeTime(record.updated_at)
                          : "—"}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      <footer className="records-footer">
        <span>
          {query.data?.total
            ? `${offset + 1}–${Math.min(offset + 100, query.data.total)} of ${query.data.total.toLocaleString()}`
            : "0 records"}
        </span>
        <span className="records-shared-note">
          Shared facts. One source of truth.
        </span>
        <div>
          <Button
            size="sm"
            variant="outline"
            disabled={offset === 0 || query.isFetching}
            onClick={() => setOffset(Math.max(0, offset - 100))}
          >
            Previous
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={
              !query.data ||
              offset + 100 >= query.data.total ||
              query.isFetching
            }
            onClick={() => setOffset(offset + 100)}
          >
            Next
            <ArrowRight />
          </Button>
        </div>
      </footer>
      {discoveryOpen && (
        <FindPeopleDialog
          ids={[...selected]}
          onClose={() => setDiscoveryOpen(false)}
        />
      )}
      {snapshotOpen && (
        <SnapshotDialog
          onClose={() => setSnapshotOpen(false)}
          onRestored={changed}
        />
      )}
      {enrollOpen && (
        <EnrollPeopleDialog
          ids={[...selected]}
          onClose={() => setEnrollOpen(false)}
          onEnrolled={changed}
        />
      )}
      {importOpen && canWrite && (
        <RecordImportDialog
          kind={kind}
          onClose={() => {
            setImportOpen(false)
            setParams(
              (previous) => {
                const next = new URLSearchParams(previous)
                next.delete("import")
                return next
              },
              { replace: true }
            )
          }}
          onImported={changed}
        />
      )}
      {editor && (
        <RecordEditor
          key={editor === "new" ? "new" : `${editor.id}:${editor.version}`}
          kind={kind}
          readOnly={!canWrite}
          record={editor === "new" ? undefined : editor}
          onClose={closeEditor}
          onSaved={changed}
        />
      )}
      {workflowOpen && workflow.data && (
        <WorkflowDialog
          kind={kind}
          workflow={workflow.data}
          onClose={() => setWorkflowOpen(false)}
          onSaved={() => void workflow.refetch()}
        />
      )}
    </div>
  )
}
