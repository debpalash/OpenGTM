import { useEffect, useMemo, useRef, useState } from "react"
import { AlertTriangle, ArrowRight, FileSpreadsheet, Loader2, Upload, X } from "lucide-react"
import type { ColumnConfig, CsvImportOptions } from "@/lib/workbook-api"

const STANDARD_FIELDS = [
  ["company", "Company"], ["website", "Website / domain"], ["email", "Email"],
  ["phone", "Phone"], ["contact_person", "Contact name"], ["contact_title", "Job title"],
  ["linkedin_url", "LinkedIn URL"], ["city", "City"], ["state", "State / region"],
  ["specialization", "Industry"], ["company_size", "Company size"],
  ["description", "Description"], ["source", "Source"], ["status", "Status"],
] as const

const ALIASES: Record<string, string> = {
  "company name": "company", organization: "company", "organization name": "company",
  "account name": "company", domain: "website", "company domain": "website",
  "website url": "website", "company website": "website", "email address": "email",
  "work email": "email", "business email": "email", "phone number": "phone",
  mobile: "phone", "mobile phone": "phone", contact: "contact_person",
  "contact name": "contact_person", "contact person": "contact_person",
  "full name": "contact_person", title: "contact_title", "job title": "contact_title",
  linkedin: "linkedin_url", "linkedin url": "linkedin_url", industry: "specialization",
  "company size": "company_size", employees: "company_size", "employee count": "company_size",
}

function normalized(value: string) {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim()
}

function suggestedTarget(header: string, columns: ColumnConfig[]) {
  const needle = normalized(header)
  const existing = columns.find(column =>
    (column.type === "lead_field" || (column.type as string) === "input")
    && [column.name, column.id, column.lead_field || ""].some(value => normalized(value) === needle),
  )
  if (existing) return existing.lead_field || existing.id
  const standard = STANDARD_FIELDS.find(([field]) => normalized(field) === needle)?.[0]
  return ALIASES[needle] || standard || `__custom__:${header}`
}

export interface CsvImportDraft {
  fileName: string
  rows: Record<string, any>[]
  fields: string[]
}

export function CsvImportDialog({ draft, columns, importing, onClose, onImport }: {
  draft: CsvImportDraft
  columns: ColumnConfig[]
  importing: boolean
  onClose: () => void
  onImport: (options: CsvImportOptions) => void
}) {
  const initialMapping = useMemo(() => Object.fromEntries(
    draft.fields.map(header => [header, suggestedTarget(header, columns)]),
  ), [draft.fields, columns])
  const [mapping, setMapping] = useState<Record<string, string>>(initialMapping)
  const [dedupe, setDedupe] = useState(true)
  const dialogRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    dialogRef.current?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !importing) onClose()
    }
    window.addEventListener("keydown", onKeyDown)
    return () => window.removeEventListener("keydown", onKeyDown)
  }, [importing, onClose])

  const mappingSummary = useMemo(() => {
    const targets = Object.values(mapping).filter(target => target !== "__skip__")
    const fieldCounts = new Map<string, number>()
    for (const target of targets) {
      if (!target.startsWith("__custom__:")) {
        fieldCounts.set(target, (fieldCounts.get(target) || 0) + 1)
      }
    }
    return {
      kept: targets.length,
      custom: targets.filter(target => target.startsWith("__custom__:")).length,
      skipped: draft.fields.length - targets.length,
      merged: [...fieldCounts.values()].some(count => count > 1),
    }
  }, [draft.fields.length, mapping])

  const submit = () => {
    const apiMapping = Object.fromEntries(Object.entries(mapping).map(([header, target]) => [
      header,
      target === "__skip__" ? null : target.replace(/^__custom__:/, ""),
    ]))
    onImport({ rows: draft.rows, mapping: apiMapping, dedupe, create_columns: true, file_name: draft.fileName })
  }

  return (
    <div className="fixed inset-0 z-[220] flex items-center justify-center bg-black/40 p-4 backdrop-blur-sm" onClick={() => { if (!importing) onClose() }}>
      <div ref={dialogRef} role="dialog" aria-modal="true" aria-labelledby="csv-import-title" aria-describedby="csv-import-summary" tabIndex={-1} className="flex max-h-[82vh] w-full max-w-2xl flex-col overflow-hidden rounded-2xl border bg-card shadow-2xl outline-none" onClick={event => event.stopPropagation()}>
        <div className="flex items-start justify-between border-b px-5 py-4">
          <div className="flex gap-3">
            <div className="flex size-9 items-center justify-center rounded-xl bg-primary/10 text-primary"><FileSpreadsheet className="size-4" /></div>
            <div>
              <h2 id="csv-import-title" className="text-sm font-semibold">Import CSV</h2>
              <p id="csv-import-summary" className="mt-0.5 text-xs text-muted-foreground">
                {draft.fileName} · {draft.rows.length.toLocaleString()} rows · {draft.fields.length.toLocaleString()} columns
              </p>
            </div>
          </div>
          <button type="button" aria-label="Close" onClick={onClose} disabled={importing} className="rounded-md p-1.5 text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-50"><X className="size-4" /></button>
        </div>

        <div className="overflow-y-auto px-5 py-3">
          <div className="mb-2 grid grid-cols-[minmax(0,1fr)_24px_minmax(0,1fr)] items-center gap-2 px-2 text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
            <span>CSV column</span><span /><span>OpenGTM column</span>
          </div>
          <div className="space-y-1.5">
            {draft.fields.map(header => (
              <div key={header} className="grid grid-cols-[minmax(0,1fr)_24px_minmax(0,1fr)] items-center gap-2 rounded-lg border bg-background/60 px-2.5 py-2">
                <div className="min-w-0">
                  <div className="truncate text-xs font-medium">{header}</div>
                  <div className="truncate text-[10px] text-muted-foreground">{draft.rows.slice(0, 3).map(row => row[header]).filter(Boolean).join(" · ") || "Empty values"}</div>
                </div>
                <ArrowRight className="size-3.5 text-muted-foreground/60" />
                <select aria-label={`Map ${header}`} value={mapping[header]} onChange={event => setMapping(current => ({ ...current, [header]: event.target.value }))} className="min-w-0 rounded-md border bg-card px-2 py-1.5 text-xs">
                  <option value={`__custom__:${header}`}>Keep as “{header}”</option>
                  <optgroup label="Lead fields">
                    {STANDARD_FIELDS.map(([field, label]) => <option key={field} value={field}>{label}</option>)}
                  </optgroup>
                  {columns.some(column => column.type === "lead_field" || (column.type as string) === "input") && <optgroup label="Existing columns">
                    {columns.filter(column => column.type === "lead_field" || (column.type as string) === "input").map(column => <option key={column.id} value={column.lead_field || column.id}>{column.name}</option>)}
                  </optgroup>}
                  <option value="__skip__">Skip column</option>
                </select>
              </div>
            ))}
          </div>
          {mappingSummary.merged && (
            <div className="mt-3 flex gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
              <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
              <span>Multiple CSV columns map to the same OpenGTM field. The first populated value in each row will be kept.</span>
            </div>
          )}
        </div>

        <div className="flex items-center justify-between gap-4 border-t bg-muted/20 px-5 py-3.5">
          <div>
            <label className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
              <input type="checkbox" checked={dedupe} onChange={event => setDedupe(event.target.checked)} className="size-3.5 rounded border" />
              Skip duplicate companies and domains
            </label>
            <p className="mt-1 text-[10px] text-muted-foreground">
              {mappingSummary.kept} kept · {mappingSummary.custom} new · {mappingSummary.skipped} skipped
            </p>
          </div>
          <button type="button" onClick={submit} disabled={importing || mappingSummary.kept === 0} className="inline-flex items-center gap-2 rounded-lg bg-primary px-3.5 py-2 text-xs font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-60">
            {importing ? <Loader2 className="size-3.5 animate-spin" /> : <Upload className="size-3.5" />}
            {importing ? "Importing…" : `Import ${draft.rows.length.toLocaleString()} rows`}
          </button>
        </div>
      </div>
    </div>
  )
}
