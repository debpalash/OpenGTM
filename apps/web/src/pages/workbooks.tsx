import { useRef, useState } from "react"
import { Link, useSearchParams } from "react-router-dom"
import { Table2, Trash2 } from "lucide-react"
import { toast } from "sonner"
import { Button, Input, AlertDialog } from "@/design-system/primitives"
import { useWorkbooks, useDeleteWorkbook } from "@/lib/workbook-hooks"
import type { Workbook } from "@/lib/workbook-api"
import { CreateWorkbookDialog } from "@/components/workbooks/create-workbook-dialog"
import { TemplateGallery } from "@/components/workbooks/template-gallery"
import "@/components/workbooks/workbooks.css"

const PAGE_SIZE = 50

export default function WorkbooksPage() {
  const query = useWorkbooks()
  const remove = useDeleteWorkbook()
  const [params, setParams] = useSearchParams()
  const [showTemplates, setShowTemplates] = useState(false)
  const [deleting, setDeleting] = useState<Workbook | null>(null)
  const [deleteError, setDeleteError] = useState("")
  const deletingRef = useRef(false)
  const deleteTrigger = useRef<HTMLButtonElement | null>(null)
  const heading = useRef<HTMLHeadingElement | null>(null)
  const search = params.get("q") ?? ""
  const status = params.get("status") ?? "all"
  const sort = params.get("sort") ?? "updated"
  const all = query.data?.workbooks ?? []
  const filtered = all.filter(workbook => (status === "all" || workbook.status === status) && workbook.name.toLowerCase().includes(search.toLowerCase()))
    .sort((left, right) => sort === "name" ? left.name.localeCompare(right.name) : (Date.parse(right.updated_at) || 0) - (Date.parse(left.updated_at) || 0))
  const pages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE))
  const page = Math.min(pages, Math.max(1, Number.parseInt(params.get("page") ?? "1", 10) || 1))
  const visible = filtered.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE)

  function setFilter(key: string, value: string) {
    // React Router's search-param callbacks do not queue like React state.
    // Read the current URL so rapid filter edits cannot restore stale values.
    const next = new URLSearchParams(window.location.search)
    if (value) next.set(key, value); else next.delete(key)
    if (key !== "page") next.delete("page")
    setParams(next, { replace: true })
  }

  async function deleteSelectedWorkbook() {
    if (!deleting || deletingRef.current) return
    deletingRef.current = true
    setDeleteError("")
    try {
      await remove.mutateAsync(deleting.id)
      setDeleting(null)
      toast.success("Workbook deleted")
    } catch (error) { setDeleteError(error instanceof Error ? error.message : "Could not delete workbook. Try again.") }
    finally { deletingRef.current = false }
  }

  return <div className="gtm-workbook-list">
    <div className="gtm-workbook-heading">
      <div><h1 ref={heading} tabIndex={-1}>Workbooks</h1><p>Research, enrich, and act on your data.</p></div>
      <CreateWorkbookDialog />
    </div>
    <div className="gtm-workbook-toolbar">
      <label className="gtm-workbook-search"><span className="sr-only">Search workbooks</span><Input value={search} onChange={event => setFilter("q", event.target.value)} placeholder="Search workbooks" /></label>
      <label>Status<select aria-label="Status" className="gtm-select" value={status} onChange={event => setFilter("status", event.target.value)}>
        <option value="all">All statuses</option>{["draft", "running", "paused", "failed", "complete"].map(value => <option key={value} value={value}>{value}</option>)}
      </select></label>
      <label>Sort<select aria-label="Sort" className="gtm-select" value={sort} onChange={event => setFilter("sort", event.target.value)}><option value="updated">Recently updated</option><option value="name">Name</option></select></label>
      <Button aria-expanded={showTemplates} aria-controls="workbook-templates" onClick={() => setShowTemplates(!showTemplates)}>{showTemplates ? "Hide templates" : "Browse templates"}</Button>
    </div>
    {showTemplates && <div id="workbook-templates"><TemplateGallery /></div>}
    {query.isPending && <div role="status" className="gtm-data-state">Loading workbooks…</div>}
    {query.isError && <div role="alert" className="gtm-data-state"><p>{query.error.message}</p><Button onClick={() => void query.refetch()} loading={query.isFetching}>Retry workbooks</Button></div>}
    {!query.isPending && !query.isError && <>
      <p className="gtm-workbook-count" role="status">{filtered.length} {filtered.length === 1 ? "workbook" : "workbooks"}{filtered.length !== all.length ? ` of ${all.length}` : ""}</p>
      {!filtered.length ? <div className="gtm-data-state">
        <Table2 aria-hidden="true" className="size-6" />
        <h2>{all.length ? "No matching workbooks" : "Your first workbook starts here"}</h2>
        <p>{all.length ? "Change your search or status filter." : "Create a workbook or choose a template above."}</p>
        {all.length > 0 && <Button onClick={() => setParams({})}>Clear filters</Button>}
      </div> : <div className="gtm-workbook-table-scroll" role="region" aria-label="Workbooks table" tabIndex={0}>
        <table className="gtm-workbook-table">
          <thead><tr><th scope="col">Workbook</th><th scope="col">Status</th><th scope="col">Rows</th><th scope="col">Columns</th><th scope="col">Processed rows</th><th scope="col">Updated</th><th scope="col"><span className="sr-only">Actions</span></th></tr></thead>
          <tbody>{visible.map(workbook => <tr key={workbook.id}>
            <td><Link to={`/workbooks/${encodeURIComponent(workbook.id)}`} className="gtm-workbook-link"><Table2 aria-hidden="true" className="size-4 shrink-0" /><span>{workbook.name}</span></Link></td>
            <td><span className="gtm-workbook-status" data-status={workbook.status}>{workbook.status}</span></td>
            <td>{workbook.total_rows.toLocaleString()}</td><td>{workbook.columns_config.length}</td>
            <td>{workbook.completed_rows.toLocaleString()} / {workbook.total_rows.toLocaleString()}</td>
            <td>{Number.isFinite(Date.parse(workbook.updated_at)) ? <time dateTime={workbook.updated_at} title={new Date(workbook.updated_at).toLocaleString()}>{new Date(workbook.updated_at).toLocaleDateString()}</time> : "—"}</td>
            <td><Button variant="ghost" aria-label={`Delete ${workbook.name}`} onClick={event => { deleteTrigger.current = event.currentTarget; setDeleteError(""); setDeleting(workbook) }}><Trash2 aria-hidden="true" className="size-4" /></Button></td>
          </tr>)}</tbody>
        </table>
      </div>}
      {pages > 1 && <nav aria-label="Workbook pages" className="gtm-workbook-pagination">
        <Button disabled={page === 1} onClick={() => setFilter("page", String(page - 1))}>Previous</Button><span>Page {page} of {pages}</span><Button disabled={page === pages} onClick={() => setFilter("page", String(page + 1))}>Next</Button>
      </nav>}
    </>}
    <AlertDialog.Root open={!!deleting} onOpenChange={(open, details) => {
      if (deletingRef.current) { details.cancel(); return }
      if (!open) setDeleting(null)
    }}>
      <AlertDialog.Popup className="gtm-workbook-dialog" finalFocus={() => deleteTrigger.current?.isConnected ? deleteTrigger.current : heading.current}>
        <AlertDialog.Header><AlertDialog.Title>Delete {deleting?.name}?</AlertDialog.Title>
        <AlertDialog.Description>This permanently deletes this workbook and its rows. This cannot be undone.</AlertDialog.Description></AlertDialog.Header>
        {deleteError && <AlertDialog.Body><p role="alert" className="text-sm text-destructive">{deleteError}</p></AlertDialog.Body>}
        <AlertDialog.Footer><AlertDialog.Close render={<Button disabled={remove.isPending} />}>Cancel</AlertDialog.Close><Button color="danger" variant="solid" loading={remove.isPending} onClick={() => void deleteSelectedWorkbook()}>Delete workbook</Button></AlertDialog.Footer>
      </AlertDialog.Popup>
    </AlertDialog.Root>
  </div>
}
