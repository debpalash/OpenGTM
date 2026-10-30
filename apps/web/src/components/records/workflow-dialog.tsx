import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { recordAPI, type RecordKind, type RecordWorkflow } from "@/lib/records"

export function WorkflowDialog({ kind, workflow, onClose, onSaved }: { kind: RecordKind; workflow: RecordWorkflow; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(workflow.name)
  const [columns, setColumns] = useState(workflow.columns)
  const [provider, setProvider] = useState("")
  const [field, setField] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const providers = useQuery({ queryKey: ["record-workflow-providers"], queryFn: ({ signal }) => recordAPI<{ providers: { name: string; capabilities: string[] }[] }>("workbooks/meta/providers", undefined, "GET", signal), staleTime: 60_000 })
  const selected = providers.data?.providers.find(p => p.name === provider)
  async function save() { setBusy(true); setError(""); try { await recordAPI(`records/${kind}/workflow`, { name, columns }, "PUT"); onSaved(); onClose() } catch (e) { setError(e instanceof Error ? e.message : "Could not save workflow") } finally { setBusy(false) } }
  return <Dialog open onOpenChange={open => { if (!open && !busy) onClose() }}><DialogContent className="sm:max-w-xl"><DialogHeader><DialogTitle>{kind === "company" ? "Company" : "Person"} enrichment workflow</DialogTitle><DialogDescription>Save the enrichment columns to use when enriching selected {kind === "company" ? "companies" : "people"}. Provider keys and pricing follow your workspace settings.</DialogDescription></DialogHeader>
    <label className="grid gap-1 text-xs font-medium">Workflow name<Input value={name} onChange={e => setName(e.target.value)} /></label>
    <div className="max-h-64 overflow-y-auto rounded-md border">{columns.length === 0 && <p className="p-3 text-xs text-muted-foreground">Add the fields you want to enrich.</p>}{columns.map((c, i) => <div key={String(c.id)} className="flex items-center justify-between gap-3 border-b p-3 text-xs"><span><strong>{String(c.name || c.target_field)}</strong><span className="ml-2 text-muted-foreground">{String(c.provider || c.type)}</span></span><Button size="sm" variant="ghost" onClick={() => setColumns(columns.filter((_, index) => index !== i))}>Remove</Button></div>)}</div>
    <div className="grid gap-2 sm:grid-cols-[1fr_1fr_auto]"><select aria-label="Enrichment provider" className="h-9 rounded-md border bg-background px-2 text-xs" value={provider} onChange={e => { setProvider(e.target.value); setField("") }}><option value="">Choose provider</option>{providers.data?.providers.map(p => <option value={p.name} key={p.name}>{p.name}</option>)}</select><select aria-label="Target field" className="h-9 rounded-md border bg-background px-2 text-xs" value={field} onChange={e => setField(e.target.value)}><option value="">Choose field</option>{selected?.capabilities.map(f => <option key={f} value={f}>{f}</option>)}</select><Button variant="outline" disabled={!provider || !field || columns.length >= 50} onClick={() => { setColumns([...columns, { id: crypto.randomUUID().replaceAll("-", ""), name: field.replaceAll("_", " "), type: "enrichment", target_field: field, provider, enabled: true }]); setField("") }}>Add</Button></div>
    <p className="text-xs text-muted-foreground">Enriching opens a linked workbook. You can add formulas, waterfalls, AI steps, and run conditions there.</p>
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}<div className="flex justify-end gap-2"><Button variant="outline" disabled={busy} onClick={onClose}>Cancel</Button><Button disabled={busy || !name.trim()} onClick={() => void save()}>Save workflow</Button></div>
  </DialogContent></Dialog>
}
