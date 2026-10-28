import { useDeferredValue, useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useNavigate } from "react-router-dom"
import { Building2, Users, Upload, Download, Plus, Sparkles, Settings2, Search, ArrowUpRight, Loader2, ListFilter } from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { RecordImportDialog } from "@/components/records/import-dialog"
import { RecordEditor } from "@/components/records/record-editor"
import { FindPeopleDialog } from "@/components/records/find-people-dialog"
import { SnapshotDialog } from "@/components/records/snapshot-dialog"
import { EnrollPeopleDialog } from "@/components/records/enroll-dialog"
import { WorkflowDialog } from "@/components/records/workflow-dialog"
import { getActiveWorkspace } from "@/lib/auth"
import { displayField, downloadData, recordAPI, type RecordKind, type RecordPage, type SharedRecord, type RecordWorkflow } from "@/lib/records"

export default function RecordsPage({ kind }: { kind: RecordKind }) {
  const navigate = useNavigate(); const client = useQueryClient(); const workspace = getActiveWorkspace()
  const [search, setSearch] = useState(""); const deferred = useDeferredValue(search)
  const [offset, setOffset] = useState(0); const [selected, setSelected] = useState<Set<string>>(new Set())
  const [discoveryOpen,setDiscoveryOpen] = useState(false)
  const [snapshotOpen,setSnapshotOpen] = useState(false); const [enrollOpen,setEnrollOpen] = useState(false)
  const [importOpen, setImportOpen] = useState(false); const [editor, setEditor] = useState<SharedRecord | "new" | null>(null)
  const [workflowOpen, setWorkflowOpen] = useState(false); const [busy, setBusy] = useState(false)
  const plural = kind === "company" ? "Companies" : "People"; const Icon = kind === "company" ? Building2 : Users
  const query = useQuery({ queryKey: ["records", workspace, kind, deferred, offset], queryFn: ({ signal }) => recordAPI<RecordPage>(`records/${kind}?limit=100&offset=${offset}&search=${encodeURIComponent(deferred)}`, undefined, "GET", signal), refetchInterval: 20_000 })
  const workflow = useQuery({ queryKey: ["record-workflow", workspace, kind], queryFn: ({ signal }) => recordAPI<RecordWorkflow>(`records/${kind}/workflow`, undefined, "GET", signal) })
  const rows = query.data?.records ?? []
  const changed = () => { void client.invalidateQueries({ queryKey: ["records"] }); void client.invalidateQueries({ queryKey: ["record-companies-picker"] }) }
  async function edit(r: SharedRecord) { try { setEditor(await recordAPI<SharedRecord>(`records/${kind}/${r.id}`)) } catch (e) { toast.error(e instanceof Error ? e.message : "Could not open record") } }
  async function workbook(run: boolean) {
    setBusy(true)
    try {
      const out = await recordAPI<{ workbook_id: string }>(`records/${kind}/workbook`, { ids: [...selected] })
      if (run && workflow.data?.columns.length) {
        try { await recordAPI(`workbooks/${out.workbook_id}/run`, {}) } catch (e) { toast.error(e instanceof Error ? e.message : "Workbook created; enrichment could not start") }
      }
      navigate(`/workbooks/${out.workbook_id}`)
    } catch (e) { toast.error(e instanceof Error ? e.message : "Could not create workbook") } finally { setBusy(false) }
  }
  async function audience() {
    setBusy(true)
    try { const out = await recordAPI<{ audience_id: string }>(`records/${kind}/audience`, { ids: [...selected], name: `${plural} · ${new Date().toLocaleDateString()}` }); await recordAPI(`audiences/${out.audience_id}/refresh`, {}); navigate("/audiences") }
    catch (e) { toast.error(e instanceof Error ? e.message : "Could not create audience") } finally { setBusy(false) }
  }
  async function exportCSV() {
    setBusy(true)
    try { const res = await fetch(`/api/records/${kind}/export`); if (!res.ok) throw new Error("Could not export records"); downloadData(await res.text(), `opengtm-${kind}.csv`, "text/csv;charset=utf-8") }
    catch (e) { toast.error(e instanceof Error ? e.message : "Export failed") } finally { setBusy(false) }
  }
  async function backup() {
    setBusy(true)
    try { const res = await fetch("/api/record-snapshots"); if (!res.ok) throw new Error("Could not export backup"); downloadData(await res.text(), "opengtm-records.json") } catch (e) { toast.error(e instanceof Error ? e.message : "Backup failed") } finally { setBusy(false) }
  }
  async function monitor() {
    setBusy(true)
    try {
      for (const id of selected) {
        const r = rows.find(item => item.id === id) ?? await recordAPI<SharedRecord>(`records/company/${id}`)
        await recordAPI("watches", { kind: "company", company_id: id, target: displayField(r.fields.domain || r.fields.name), interval: "daily" })
      }
      toast.success("Company watches created"); navigate("/watches")
    } catch (e) { toast.error(e instanceof Error ? e.message : "Could not create watches") } finally { setBusy(false) }
  }
  function toggle(id: string) { setSelected(previous => { const next = new Set(previous); if (next.has(id)) next.delete(id); else next.add(id); return next }) }
  return <div className="flex h-full min-h-0 flex-col">
    <div className="flex flex-wrap items-center justify-between gap-4 border-b px-6 py-5"><div className="flex items-start gap-3"><span className="rounded-md border bg-muted/40 p-2"><Icon className="size-5 text-primary" /></span><div><h1 className="text-lg font-semibold tracking-tight">{plural}</h1><p className="mt-1 text-xs text-muted-foreground">{kind === "company" ? "Build your company universe. Enrich and qualify accounts before finding people." : "Independent people, linked companies, and a history that follows every move."}</p></div></div><div className="flex flex-wrap gap-2"><Button size="sm" variant="outline" onClick={() => setImportOpen(true)}><Upload className="size-4" />Import from Clay</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void exportCSV()}><Download className="size-4" />Export</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void backup()}>Backup JSON</Button><Button size="sm" variant="ghost" onClick={() => setSnapshotOpen(true)}>Restore backup</Button><Button size="sm" onClick={() => setEditor("new")}><Plus className="size-4" />New {kind}</Button></div></div>
    <div className="flex flex-wrap items-center gap-3 border-b px-6 py-3"><div className="flex rounded-md border p-0.5 text-xs"><Link to="/companies" className={`rounded px-3 py-1.5 ${kind === "company" ? "bg-primary/10 font-semibold text-primary" : "text-muted-foreground hover:bg-muted"}`}>Companies</Link><Link to="/people" className={`rounded px-3 py-1.5 ${kind === "person" ? "bg-primary/10 font-semibold text-primary" : "text-muted-foreground hover:bg-muted"}`}>People</Link></div><div className="relative min-w-40 max-w-sm flex-1"><Search className="absolute left-2.5 top-2.5 size-3.5 text-muted-foreground" /><Input aria-label={`Search ${plural.toLowerCase()}`} className="h-8 pl-8 text-xs" placeholder="Search names, domains, and custom fields…" value={search} onChange={e => { setSearch(e.target.value); setOffset(0); setSelected(new Set()) }} /></div><span className="text-xs tabular-nums text-muted-foreground">{query.data?.total.toLocaleString() ?? "…"} records</span><div className="flex-1" /><Button size="sm" variant="ghost" disabled={!workflow.data} onClick={() => setWorkflowOpen(true)}><Settings2 className="size-3.5" />Enrichment workflow</Button></div>
    {selected.size > 0 && <div className="flex flex-wrap items-center gap-2 border-b bg-primary/5 px-6 py-2"><span className="mr-2 text-xs font-medium">{selected.size} selected</span><Button size="sm" disabled={busy} onClick={() => void workbook(true)}><Sparkles className="size-3.5" />{workflow.data?.columns.length ? "Enrich selected" : "Open enrichment workbook"}</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void audience()}><ListFilter className="size-3.5" />Create audience</Button>{kind === "company" ? <><Button size="sm" variant="outline" disabled={busy || selected.size > 100} onClick={() => setDiscoveryOpen(true)}>Find people</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => void monitor()}>Monitor companies</Button></> : <Button size="sm" variant="outline" disabled={busy} onClick={() => setEnrollOpen(true)}>Enroll in outreach</Button>}<Button size="sm" variant="ghost" onClick={() => setSelected(new Set())}>Clear selection</Button>{busy && <Loader2 className="size-4 animate-spin" />}</div>}
    <div className="min-h-0 flex-1 overflow-auto">
      {query.isPending ? <p role="status" className="p-12 text-center text-sm text-muted-foreground">Loading {plural.toLowerCase()}…</p> : query.isError ? <div role="alert" className="space-y-3 p-12 text-center text-sm"><p>{query.error.message}</p><Button variant="outline" onClick={() => void query.refetch()}>Try again</Button></div> : rows.length === 0 ? <div className="mx-auto flex max-w-md flex-col items-center gap-4 px-6 py-20 text-center"><Icon className="size-10 text-muted-foreground" /><h2 className="text-lg font-semibold">{search ? "No matching records" : `Your ${plural.toLowerCase()} belong here`}</h2><p className="text-sm text-muted-foreground">{search ? "Try another name, domain, or field value." : "Bring a Clay CSV or create your first record. Linked workbooks share the same accepted facts."}</p>{!search && <Button onClick={() => setImportOpen(true)}><Upload className="size-4" />Import a table</Button>}</div> : <table className="w-full border-collapse text-left text-xs"><thead className="sticky top-0 z-10 bg-muted/95"><tr className="h-10 border-b text-muted-foreground"><th className="w-10 px-4"><input type="checkbox" aria-label="Select page" checked={rows.length > 0 && rows.every(r => selected.has(r.id))} onChange={e => setSelected(previous => { const next = new Set(previous); for (const r of rows) { if (e.target.checked) next.add(r.id); else next.delete(r.id) } return next })} /></th><th className="min-w-48 px-4 font-medium">{kind === "company" ? "Company" : "Person"}</th><th className="min-w-48 px-4 font-medium">{kind === "company" ? "Domain" : "Company"}</th><th className="min-w-48 px-4 font-medium">{kind === "company" ? "Industry" : "Email"}</th><th className="min-w-32 px-4 font-medium">{kind === "company" ? "Location" : "Phone"}</th><th className="w-20 px-4 font-medium">Score</th><th className="w-10" /></tr></thead><tbody>{rows.map(r => <tr key={r.id} className={`h-12 border-b transition-colors hover:bg-muted/40 ${selected.has(r.id) ? "bg-primary/5" : ""}`}><td className="px-4"><input type="checkbox" aria-label={`Select ${displayField(r.fields.name || r.fields.full_name)}`} checked={selected.has(r.id)} onChange={() => toggle(r.id)} /></td><td className="px-4"><button className="flex max-w-72 items-center gap-2 truncate text-left font-medium hover:text-primary" onClick={() => void edit(r)}>{displayField(r.fields.name || r.fields.full_name) || "Unnamed record"}</button></td><td className="max-w-72 truncate px-4 text-muted-foreground">{displayField(kind === "company" ? r.fields.domain : r.company?.name) || "—"}</td><td className="max-w-72 truncate px-4 text-muted-foreground">{displayField(kind === "company" ? r.fields.industry : r.fields.email) || "—"}</td><td className="px-4 text-muted-foreground">{displayField(kind === "company" ? r.fields.city : r.fields.phone) || "—"}</td><td className="px-4 font-mono tabular-nums">{displayField(r.fields.score) || "—"}</td><td><button aria-label={`Edit ${displayField(r.fields.name || r.fields.full_name)}`} className="p-3 text-muted-foreground hover:text-primary" onClick={() => void edit(r)}><ArrowUpRight className="size-3.5" /></button></td></tr>)}</tbody></table>}
    </div>
    <div className="flex items-center justify-between border-t px-6 py-3 text-xs text-muted-foreground"><span>Shared accepted records · updates appear in linked workbooks</span><div className="flex items-center gap-3"><Button size="sm" variant="outline" disabled={offset === 0 || query.isPending} onClick={() => setOffset(Math.max(0, offset - 100))}>Previous</Button><span>{query.data?.total ? `${offset + 1}–${Math.min(offset + 100, query.data.total)}` : "0"}</span><Button size="sm" variant="outline" disabled={!query.data || offset + 100 >= query.data.total || query.isPending} onClick={() => setOffset(offset + 100)}>Next</Button></div></div>
    {discoveryOpen && <FindPeopleDialog ids={[...selected]} onClose={() => setDiscoveryOpen(false)} />}
    {snapshotOpen && <SnapshotDialog onClose={() => setSnapshotOpen(false)} onRestored={changed} />}
    {enrollOpen && <EnrollPeopleDialog ids={[...selected]} onClose={() => setEnrollOpen(false)} onEnrolled={() => { setEnrollOpen(false); toast.success("People enrolled"); navigate("/outreach") }} />}
    {importOpen && <RecordImportDialog kind={kind} onClose={() => setImportOpen(false)} onImported={changed} />}
    {editor && <RecordEditor key={editor === "new" ? "new" : `${editor.id}:${editor.version}`} kind={kind} record={editor === "new" ? undefined : editor} onClose={() => setEditor(null)} onSaved={changed} />}
    {workflowOpen && workflow.data && <WorkflowDialog kind={kind} workflow={workflow.data} onClose={() => setWorkflowOpen(false)} onSaved={() => void workflow.refetch()} />}
  </div>
}
