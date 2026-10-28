import { useState } from "react"
import { Upload, ArrowRight, CheckCircle2, FileSpreadsheet, Loader2 } from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { recordImportAPI, downloadData, type RecordKind, type RecordImport, type ImportResult } from "@/lib/records"

export function RecordImportDialog({ kind, onClose, onImported }: { kind: RecordKind; onClose: () => void; onImported: () => void }) {
  const [request, setRequest] = useState<RecordImport>({ record_type: kind, source: "clay:prospects", idempotency_key: crypto.randomUUID() })
  const [rawRows, setRawRows] = useState("")
  const [filename, setFilename] = useState("")
  const [result, setResult] = useState<ImportResult | null>(null)
  const [mapping, setMapping] = useState<Record<string, string>>({})
  const [reviewed, setReviewed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const complete = result && !result.preview
  async function load(file?: File) {
    if (!file) return
    setError(""); setResult(null); setMapping({}); setReviewed(false); setRawRows("")
    if (file.size > 15 * 1024 * 1024) { setError("Use files up to 15 MB and 10,000 rows. Split larger exports into batches."); return }
    try {
      const text = await file.text()
      const data = file.name.toLowerCase().endsWith(".json") ? { rows: JSON.parse(text) as Record<string, unknown>[] } : { csv: text }
      if ("rows" in data && !Array.isArray(data.rows)) throw new Error("JSON must be an array of row objects.")
      if ("rows" in data) setRawRows(text)
      setRequest({ record_type: kind, source: request.source, idempotency_key: crypto.randomUUID(), ...data }); setFilename(file.name)
    } catch (e) { setError(e instanceof Error ? e.message : "Could not read the file") }
  }
  async function run(preview: boolean) {
    setBusy(true); setError("")
    try {
      const output = await recordImportAPI({ ...request, ...(Object.keys(mapping).length ? { mapping } : {}) }, preview, rawRows)
      setResult(output); setMapping(output.mapping); setReviewed(preview)
      if (!preview) { onImported(); toast.success(`${output.imported} records imported${output.replayed ? " (previous import recovered)" : ""}`) }
    } catch (e) { setError(e instanceof Error ? e.message : "Import failed. Retry with the same file to recover this batch.") }
    finally { setBusy(false) }
  }
  const targets = ["", "external_id", "company_id", "company_external_id", "company_source", "company.name", "company.domain", "company.website", "company.industry", "company.city", "company.country", "company.employee_count_exact", ...(kind === "person" ? ["person.full_name", "person.first_name", "person.last_name", "person.email", "person.phone", "person.linkedin_url", "employment.title", "employment.department", "employment.seniority"] : ["company.email", "company.phone", "company.linkedin_url"])]
  return <Dialog open onOpenChange={open => { if (!open && !busy) onClose() }}>
    <DialogContent className="sm:max-w-3xl max-h-[90vh] overflow-y-auto">
      <DialogHeader><DialogTitle>Bring your {kind === "company" ? "companies" : "people"} from Clay</DialogTitle><DialogDescription>Export a Clay table as CSV, then match its columns here. Custom fields and JSON values come with you.</DialogDescription></DialogHeader>
      {!complete && <>
        <label className="flex cursor-pointer items-center gap-4 rounded-lg border-2 border-dashed border-border p-5 hover:bg-muted/40">
          <FileSpreadsheet className="size-8 text-primary" /><span className="flex-1"><strong className="block">{filename || "Choose a CSV or JSON file"}</strong><span className="text-xs text-muted-foreground">Up to 10,000 rows per batch · UTF-8 · CSV or a JSON array</span></span><Upload className="size-4" />
          <input aria-label="Import file" type="file" accept=".csv,.json,text/csv,application/json" className="sr-only" disabled={busy} onChange={e => void load(e.target.files?.[0])} />
        </label>
        <label className="grid gap-1 text-xs font-medium">Source table name<Input value={request.source} disabled={busy} onChange={e => { setRequest({ ...request, source: e.target.value }); setReviewed(false) }} /><span className="font-normal text-muted-foreground">Use the same name when updating this table later. Row IDs are scoped to this source.</span></label>
        {Object.keys(mapping).length > 0 && <div className="overflow-hidden rounded-md border"><div className="grid grid-cols-[1fr_24px_1fr] gap-2 bg-muted/50 p-3 text-xs font-semibold"><span>Source column</span><span /><span>OpenGTM field</span></div>
          <datalist id="record-mapping-fields">{targets.filter(Boolean).map(t => <option value={t} key={t} />)}</datalist>
          <div className="max-h-60 overflow-auto">{Object.entries(mapping).map(([header, target]) => <div key={header} className="grid grid-cols-[1fr_24px_1fr] items-center gap-2 border-t px-3 py-2 text-xs"><span className="truncate" title={header}>{header}</span><ArrowRight className="size-3 text-muted-foreground" /><Input aria-label={`Map ${header}`} list="record-mapping-fields" className="h-8 text-xs" value={target} placeholder="Ignore column" disabled={busy} onChange={e => { setMapping({ ...mapping, [header]: e.target.value }); setReviewed(false) }} /></div>)}</div>
          <p className="border-t p-3 text-xs text-muted-foreground">Use company.*, person.* or employment.* for a custom field. Clear a mapping to ignore it.</p>
        </div>}
      </>}
      {result && <div className="space-y-3 rounded-md border p-4">
        <div className="flex items-center gap-2 font-medium">{complete && <CheckCircle2 className="size-5 text-emerald-600" />}{complete ? "Import complete" : "Preview"}: {result.imported.toLocaleString()} accepted · {result.rejected.toLocaleString()} rejected</div>
        {result.warnings.map(warning => <p key={warning} className="text-xs text-muted-foreground">{warning}</p>)}
        {result.rejected > 0 && <div className="max-h-32 overflow-auto text-xs text-destructive">{result.rows.filter(row => row.error).map(row => <p key={row.row} className="py-1">Row {row.row}: {row.error}</p>)}</div>}
        <Button variant="ghost" size="sm" onClick={() => downloadData(JSON.stringify(result, null, 2), "import-report.json")}>Download row report</Button>
      </div>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <div className="flex justify-end gap-2"><Button variant="outline" disabled={busy} onClick={onClose}>{complete ? "Done" : "Cancel"}</Button>{!complete && <><Button variant="outline" disabled={busy || !filename || !request.source.trim()} onClick={() => void run(true)}>{busy && <Loader2 className="size-4 animate-spin" />}Preview & validate</Button><Button disabled={busy || !reviewed || !result?.imported} onClick={() => void run(false)}>Import {result?.imported ? `${result.imported} records` : "records"}</Button></>}</div>
    </DialogContent>
  </Dialog>
}
