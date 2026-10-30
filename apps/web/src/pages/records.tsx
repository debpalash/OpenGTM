import { RecordTable } from "@/features/records/collection/record-table"
import { ColumnConfiguration } from "@/features/records/collection/column-configuration"
import { useColumnLayout } from "@/features/records/collection/use-column-layout"
import { useUrlSearchInput } from "@/lib/use-url-search-input"
import { useDeferredValue, useState, useRef } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { useNavigate, useSearchParams } from "react-router-dom"
import {
  Upload,
  Download,
  Plus,
  Sparkles,
  Settings2,
  Search,
  Loader2,
  ListFilter,
  MoreHorizontal,
  ChevronDown,
  ArrowRight,
  SlidersHorizontal
} from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { RecordImportDialog } from "@/components/records/import-dialog"
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription
} from "@/design-system/primitives"
import { RecordEditor } from "@/components/records/record-editor"
import { FindPeopleDialog } from "@/components/records/find-people-dialog"
import { SnapshotDialog } from "@/components/records/snapshot-dialog"
import { EnrollPeopleDialog } from "@/components/records/enroll-dialog"
import { WorkflowDialog } from "@/components/records/workflow-dialog"
import {
  PageHeader,
  ErrorState,
  ViewState
} from "@/design-system/patterns/workbench"
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem
} from "@/components/ui/dropdown-menu"
import "./records.css"
import { getActiveWorkspace } from "@/lib/auth"
import {
  displayField,
  fetchSharedRecord,
  fetchRecordPage,
  downloadData,
  recordAPI,
  type RecordKind,
  type SharedRecord,
  type RecordWorkflow
} from "@/lib/records"

export default function RecordsPage({ kind }: { kind: RecordKind }) {
  const columns = useColumnLayout(kind)
  const navigate = useNavigate()
  const client = useQueryClient()
  const workspace = getActiveWorkspace()
  const [params, setParams] = useSearchParams()
  const search = params.get("q") || ""
  const searchInput = useUrlSearchInput(search, (value) => setView("q", value))
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
  const [density, setDensity] = useState(() => {
    try {
      const saved = localStorage.getItem("opengtm-record-density")
      return saved && ["compact", "default", "comfortable"].includes(saved)
        ? saved
        : "compact"
    } catch {
      return "compact"
    }
  })
  const changeDensity = (value: string) => {
    if (!["compact", "default", "comfortable"].includes(value)) return
    setDensity(value)
    try {
      localStorage.setItem("opengtm-record-density", value)
    } catch {
      /* In-memory preference remains available. */
    }
  }
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
  const [editor, setEditor] = useState<"new" | null>(null)
  const recordID = params.get("record")
  const inspected = useQuery({
    queryKey: ["record-inspect", workspace, kind, recordID],
    queryFn: ({ signal }) => fetchSharedRecord(kind, recordID!, signal),
    enabled: !!recordID,
    staleTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false
  })
  const [workflowOpen, setWorkflowOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const plural = kind === "company" ? "Companies" : "People"
  const query = useQuery({
    queryKey: ["records", workspace, kind, deferred, offset, filter, sort],
    queryFn: ({ signal }) =>
      fetchRecordPage(
        kind,
        {
          limit: 100,
          offset,
          search: deferred,
          filter,
          sort,
          direction: sort === "name" ? "asc" : "desc"
        },
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
    void client.invalidateQueries({ queryKey: ["record-history"] })
    void client.invalidateQueries({ queryKey: ["company-people"] })
    void client.invalidateQueries({ queryKey: ["record-companies-picker"] })
    void client.invalidateQueries({ queryKey: ["record-overview"] })
  }
  function edit(record: SharedRecord, trigger: HTMLButtonElement) {
    returnFocus.current = trigger
    setEditor(null)
    setParams((previous) => {
      const next = new URLSearchParams(previous)
      next.set("record", record.id)
      return next
    })
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
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous)
        next.delete("record")
        return next
      },
      { replace: true }
    )
    requestAnimationFrame(() => {
      const target = returnFocus.current?.isConnected
        ? returnFocus.current
        : document.querySelector<HTMLElement>("[data-page-title]")
      target?.focus()
    })
  }
  const checkedOnPage = rows.filter((row) => selected.has(row.id)).length
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
              <span className="records-import-label-wide">
                Import from Clay
              </span>
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
          <ColumnConfiguration kind={kind} layout={columns.layout} temporary={columns.temporary} onApply={columns.apply} />
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
              <DropdownMenuRadioGroup
                value={density}
                onValueChange={(value) => changeDensity(String(value))}
              >
                <DropdownMenuRadioItem value="compact">
                  Compact rows
                </DropdownMenuRadioItem>
                <DropdownMenuRadioItem value="default">
                  Standard rows
                </DropdownMenuRadioItem>
                <DropdownMenuRadioItem value="comfortable">
                  Comfortable rows
                </DropdownMenuRadioItem>
              </DropdownMenuRadioGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      {columns.temporary && <p role="status" className="record-column-preference-notice">Column changes apply to this view. This browser could not save the preference for your next visit.</p>}
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
          <RecordTable
            kind={kind}
            records={rows}
            total={query.data.total}
            density={density}
            layout={columns.layout}
            selected={selected}
            onToggle={toggle}
            onInspect={edit}
            onTogglePage={checked => setSelected(previous => {
              const next = new Set(previous)
              for (const record of rows) {
                if (checked) next.add(record.id)
                else next.delete(record.id)
              }
              return next
            })}
          />
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
          key={`${workspace}:${kind}`}
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
          onOpenRecord={(id) => {
            setImportOpen(false)
            setParams((previous) => {
              const next = new URLSearchParams(previous)
              next.delete("import")
              next.set("record", id)
              return next
            })
          }}
          onSelectImported={(ids) => {
            setSelected(new Set(ids))
            setImportOpen(false)
            setParams((previous) => {
              const next = new URLSearchParams(previous)
              next.delete("import")
              return next
            }, { replace: true })
            toast.success(`${ids.length.toLocaleString()} records selected. Review enrichment in a linked workbook.`)
          }}
        />
      )}
      {editor && (
        <RecordEditor
          key="new"
          kind={kind}
          readOnly={!canWrite}
          onClose={closeEditor}
          onSaved={changed}
        />
      )}
      {recordID &&
        !editor &&
        !inspected.data &&
        (inspected.isPending || inspected.isError) && (
          <Sheet
            open
            onOpenChange={(open) => {
              if (!open) closeEditor()
            }}
          >
            <SheetContent className="record-inspector">
              <SheetHeader>
                <SheetTitle>
                  {inspected.isError ? "Record unavailable" : "Opening record…"}
                </SheetTitle>
                <SheetDescription>
                  {inspected.isError
                    ? "The record may have moved or your access may have changed."
                    : "Loading accepted facts and source history."}
                </SheetDescription>
              </SheetHeader>
              {inspected.isError ? (
                <ErrorState
                  error={inspected.error}
                  retry={() => void inspected.refetch()}
                />
              ) : (
                <ViewState kind="loading" title="Loading record…" />
              )}
            </SheetContent>
          </Sheet>
        )}
      {recordID && !editor && inspected.data && (
        <RecordEditor
          key={`${workspace}:${kind}:${inspected.data.id}`}
          kind={kind}
          readOnly={!canWrite}
          record={inspected.data}
          refreshing={inspected.isFetching}
          refreshError={inspected.isError ? inspected.error : undefined}
          onRetry={() => void inspected.refetch()}
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
