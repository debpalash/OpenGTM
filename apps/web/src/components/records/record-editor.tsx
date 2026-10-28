import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { getActiveWorkspace } from "@/lib/auth"
import { Link } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { displayField, recordAPI, type RecordKind, type SharedRecord, type RecordPage } from "@/lib/records"

export function RecordEditor({ kind, record, onClose, onSaved }: { kind: RecordKind; record?: SharedRecord; onClose: () => void; onSaved: () => void }) {
  const [fields, setFields] = useState<Record<string, string>>(() => Object.fromEntries(Object.entries({ ...(kind === "company" ? { name: "", domain: "", industry: "", city: "", description: "" } : { full_name: "", email: "", linkedin_url: "", phone: "" }), ...record?.fields }).map(([key, value]) => [key, displayField(value)])))
  const [companyID, setCompanyID] = useState(record?.company_id ?? "")
  const [title, setTitle] = useState(displayField(record?.employments?.find(e => e.current)?.fields.title))
  const [historyOpen,setHistoryOpen] = useState(false)
  const history = useQuery({ queryKey:["record-history",getActiveWorkspace(),kind,record?.id],queryFn:({signal})=>recordAPI<{id:number;record_type:string;field:string;value:unknown;source:string;observed_at:string;accepted:boolean}[]>(`records/${kind}/${record!.id}/history`,undefined,"GET",signal),enabled:!!record && historyOpen })
  const [custom, setCustom] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const companies = useQuery({ queryKey: ["record-companies-picker",getActiveWorkspace()], queryFn: ({ signal }) => recordAPI<RecordPage>("records/company?limit=500", undefined, "GET", signal), enabled: kind === "person" })
  async function save() {
    setError(""); setBusy(true)
    try {
      const data: Record<string, unknown> = {}
      for (const [key, value] of Object.entries(fields)) {
        const old = record?.fields[key]
        if (record && value === displayField(old)) continue
        data[key] = typeof old === "object" && old !== null ? JSON.parse(value) : typeof old === "number" ? (value === "" ? null : Number(value)) : typeof old === "boolean" ? value === "true" : value
      }
      const body = kind === "company" ? { company: data } : { person: data, ...(companyID ? { company_id: companyID } : record?.company_id ? { end_employment: true } : {}), ...(title !== displayField(record?.employments?.find(e => e.current)?.fields.title) || (!record && title) ? { employment: { title } } : {}) }
      await recordAPI(`records/${kind}${record ? `/${record.id}` : ""}`, record ? { ...body, version: record.version } : body, record ? "PATCH" : "POST")
      onSaved(); onClose()
    } catch (e) { setError(e instanceof Error ? e.message : "Could not save record") }
    finally { setBusy(false) }
  }
  return <Dialog open onOpenChange={open => { if (!open && !busy) onClose() }}><DialogContent className="sm:max-w-xl max-h-[90vh] overflow-y-auto">
    <DialogHeader><DialogTitle>{record ? "Edit" : "New"} {kind}</DialogTitle><DialogDescription>Accepted fields are shared with every linked workbook and audience.</DialogDescription></DialogHeader>
    <form className="space-y-4" onSubmit={e => { e.preventDefault(); void save() }}>
      {Object.entries(fields).map(([key, value]) => <label key={key} className="grid gap-1 text-xs font-medium"><span>{key.replaceAll("_", " ")}</span>{key === "description" || value.length > 120 ? <Textarea aria-label={key} value={value} disabled={busy} onChange={e => setFields({ ...fields, [key]: e.target.value })} /> : <Input aria-label={key} value={value} disabled={busy} onChange={e => setFields({ ...fields, [key]: e.target.value })} />}</label>)}
      <div className="flex gap-2"><Input aria-label="Custom field name" placeholder="Custom field name" value={custom} onChange={e => setCustom(e.target.value)} /><Button type="button" variant="outline" disabled={!custom.trim() || custom in fields} onClick={() => { setFields({ ...fields, [custom.trim()]: "" }); setCustom("") }}>Add field</Button></div>
      {kind === "person" && <div className="space-y-3 border-t pt-4"><label className="grid gap-1 text-xs font-medium">Current company<select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={companyID} onChange={e => setCompanyID(e.target.value)} disabled={busy}><option value="">No company selected</option>{companies.data?.records.map(c => <option key={c.id} value={c.id}>{displayField(c.fields.name)} · {displayField(c.fields.domain)}</option>)}</select></label><label className="grid gap-1 text-xs font-medium">Current role<Input value={title} onChange={e => setTitle(e.target.value)} /></label><p className="text-xs text-muted-foreground">Changing companies keeps this person’s identity and previous employment.</p></div>}
      {record?.employments && record.employments.length > 0 && <div className="border-t pt-3"><h3 className="mb-2 text-xs font-semibold">Employment history</h3>{record.employments.map(e => <div key={e.id} className="flex justify-between gap-4 py-2 text-xs"><span>{displayField(e.fields.title) || "Role unknown"} · {e.company_name || record.company?.name as string || "Company"}</span><span className={e.current ? "text-emerald-600" : "text-muted-foreground"}>{e.current ? "Current" : "Previous"}</span></div>)}</div>}
      {record && <details className="border-t pt-3 text-xs" onToggle={e => setHistoryOpen(e.currentTarget.open)}><summary className="cursor-pointer font-semibold">Field observations & decisions</summary>{history.isPending && historyOpen && <p className="p-2 text-muted-foreground">Loading history…</p>}{history.isError && <p role="alert">Could not load field history.</p>}<div className="max-h-52 overflow-auto">{history.data?.map(o => <div key={o.id} className="space-y-1 border-b py-2"><div className="flex justify-between gap-2"><strong>{o.field.replaceAll("_", " ")}</strong><span className={o.accepted ? "text-emerald-600" : "text-amber-600"}>{o.accepted ? "Accepted" : "Observed"}</span></div><p className="break-words text-muted-foreground">{displayField(o.value)}</p><div className="flex items-center justify-between gap-2"><span className="text-[10px] text-muted-foreground">{o.source} · {o.observed_at.split(" ")[0]}</span>{o.record_type === kind && o.field !== "current_employment" && <button type="button" className="text-primary hover:underline" onClick={() => setFields({ ...fields, [o.field]: displayField(o.value) })}>Use value</button>}</div></div>)}</div></details>}
      {record?.external_ids && record.external_ids.length > 0 && <details className="text-xs text-muted-foreground"><summary className="cursor-pointer">Source identities</summary>{record.external_ids.map(i => <p key={`${i.source}:${i.external_id}`} className="break-all py-1">{i.source} / {i.external_id}</p>)}</details>}
      {kind === "person" && record?.company_id && <Link className="text-xs text-primary underline" to="/companies" onClick={onClose}>View shared company records</Link>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <div className="flex justify-end gap-2"><Button type="button" variant="outline" disabled={busy} onClick={onClose}>Cancel</Button><Button type="submit" disabled={busy}>{busy ? "Saving…" : "Save shared record"}</Button></div>
    </form>
  </DialogContent></Dialog>
}
