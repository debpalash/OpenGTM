import { useEffect, useState } from "react"
import {
  ArrowRight,
  ArrowLeft,
  CheckCircle2,
  FileSpreadsheet,
  Loader2,
  Download
} from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription
} from "@/components/ui/dialog"
import { getActiveWorkspace } from "@/lib/auth"
import {
  recordImportAPI,
  downloadData,
  type RecordKind,
  type RecordImport,
  type ImportResult
} from "@/lib/records"
import "./import-dialog.css"
interface Draft {
  key: string
  hash: string
  filename: string
  source: string
  mapping: Record<string, string>
}
function readDraft(key: string): Draft | null {
  try {
    const d = JSON.parse(sessionStorage.getItem(key) || "null")
    return d &&
      typeof d.key === "string" &&
      typeof d.hash === "string" &&
      typeof d.filename === "string" &&
      typeof d.source === "string" &&
      d.mapping &&
      typeof d.mapping === "object"
      ? d
      : null
  } catch {
    return null
  }
}
export function RecordImportDialog({
  kind,
  onClose,
  onImported
}: {
  kind: RecordKind
  onClose: () => void
  onImported: () => void
}) {
  const storageKey = `opengtm-import:${getActiveWorkspace()}:${kind}`
  const [recovery, setRecovery] = useState(() => readDraft(storageKey))
  const [request, setRequest] = useState<RecordImport>({
    record_type: kind,
    source: recovery?.source || "clay:prospects",
    idempotency_key: recovery?.key || crypto.randomUUID()
  })
  const [rawRows, setRawRows] = useState("")
  const [filename, setFilename] = useState("")
  const [hash, setHash] = useState("")
  const [result, setResult] = useState<ImportResult | null>(null)
  const [mapping, setMapping] = useState<Record<string, string>>({})
  const [step, setStep] = useState(0)
  const [busy, setBusy] = useState(false)
  const [elapsed, setElapsed] = useState(0)
  const [error, setError] = useState("")
  useEffect(() => {
    if (!busy) return
    const started = Date.now()
    const timer = setInterval(
      () => setElapsed(Math.floor((Date.now() - started) / 1000)),
      1000
    )
    return () => clearInterval(timer)
  }, [busy])
  const complete = step === 3
  function forget() {
    setRecovery(null)
    try {
      sessionStorage.removeItem(storageKey)
    } catch {
      /* Storage may be unavailable. */
    }
  }
  async function load(file?: File) {
    if (!file) return
    setError("")
    setResult(null)
    setFilename("")
    setRawRows("")
    setMapping({})
    setStep(0)
    if (file.size > 15 * 1024 * 1024) {
      setError(
        "Use files up to 15 MB and 10,000 rows. Split larger exports into batches."
      )
      return
    }
    try {
      const text = await file.text()
      const data = file.name.toLowerCase().endsWith(".json")
        ? { rows: JSON.parse(text) as Record<string, unknown>[] }
        : { csv: text }
      if (
        "rows" in data &&
        (!Array.isArray(data.rows) ||
          data.rows.some(
            (row) => !row || typeof row !== "object" || Array.isArray(row)
          ))
      )
        throw new Error("JSON must be an array of row objects.")
      const digest = await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(text)
      )
      const fingerprint = Array.from(new Uint8Array(digest), (v) =>
        v.toString(16).padStart(2, "0")
      ).join("")
      const same = recovery?.hash === fingerprint
      setHash(fingerprint)
      setRawRows("rows" in data ? text : "")
      setFilename(file.name)
      setMapping(same ? recovery.mapping : {})
      setRequest({
        record_type: kind,
        source: same ? recovery.source : request.source,
        idempotency_key: same ? recovery.key : crypto.randomUUID(),
        ...data
      })
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : "Could not read the file."
      )
    }
  }
  async function preview(next: number) {
    setBusy(true)
    setElapsed(0)
    setError("")
    try {
      const output = await recordImportAPI(
        { ...request, ...(Object.keys(mapping).length ? { mapping } : {}) },
        true,
        rawRows
      )
      setResult(output)
      setMapping(output.mapping)
      setStep(next)
    } catch (cause) {
      setError(
        cause instanceof Error
          ? cause.message
          : "Could not validate this file. Your selections are kept."
      )
    } finally {
      setBusy(false)
    }
  }
  async function commit() {
    const draft = {
      key: request.idempotency_key,
      hash,
      filename,
      source: request.source,
      mapping
    }
    setRecovery(draft)
    try {
      sessionStorage.setItem(storageKey, JSON.stringify(draft))
    } catch {
      /* In-memory retry retains this key. */
    }
    setBusy(true)
    setElapsed(0)
    setError("")
    try {
      const output = await recordImportAPI(
        { ...request, mapping },
        false,
        rawRows
      )
      setResult(output)
      setStep(3)
      forget()
      onImported()
      toast.success(
        `${output.imported.toLocaleString()} records imported${output.replayed ? " (completed batch recovered)" : ""}`
      )
    } catch (cause) {
      setError(
        `${cause instanceof Error ? cause.message : "The response was lost."} Check the batch status or retry. This batch keeps the same import key.`
      )
    } finally {
      setBusy(false)
    }
  }
  async function recover() {
    if (!recovery) return
    setBusy(true)
    setElapsed(0)
    setError("")
    try {
      const response = await fetch(
        `/api/record-imports/${encodeURIComponent(recovery.key)}`
      )
      if (response.status === 404)
        throw new Error(
          `No completed receipt is available yet. Reselect ${recovery.filename} and retry with the same batch key. An import may still be running.`
        )
      if (!response.ok)
        throw new Error("Could not check this batch. Try again.")
      setFilename(recovery.filename)
      setResult({ ...((await response.json()) as ImportResult), replayed: true })
      setStep(3)
      forget()
      onImported()
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : "Could not check this batch."
      )
    } finally {
      setBusy(false)
    }
  }
  const targets = [
    "external_id",
    "company_id",
    "company_external_id",
    "company_source",
    "company.name",
    "company.domain",
    "company.website",
    "company.industry",
    "company.city",
    "company.country",
    "company.employee_count_exact",
    ...(kind === "person"
      ? [
          "person.full_name",
          "person.first_name",
          "person.last_name",
          "person.email",
          "person.phone",
          "person.linkedin_url",
          "employment.title",
          "employment.department",
          "employment.seniority"
        ]
      : ["company.email", "company.phone", "company.linkedin_url"])
  ]
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onClose()
      }}
    >
      <DialogContent className="record-import-dialog">
        <DialogHeader>
          <DialogTitle>
            Import {kind === "company" ? "companies" : "people"}
          </DialogTitle>
          <DialogDescription>
            Bring a Clay table or CSV into your shared record library.
          </DialogDescription>
        </DialogHeader>
        <ol className="record-import-steps" aria-label="Import progress">
          {["Choose file", "Map columns", "Review", "Results"].map(
            (label, index) => (
              <li
                key={label}
                aria-current={step === index ? "step" : undefined}
                data-complete={step > index}
              >
                <span>
                  {step > index ? (
                    <CheckCircle2 aria-hidden="true" />
                  ) : (
                    index + 1
                  )}
                </span>
                {label}
              </li>
            )
          )}
        </ol>
        <div className="record-import-body" aria-busy={busy}>
          {recovery && !complete && (
            <div className="record-import-recovery">
              <strong>Recover your last batch</strong>
              <p>
                {recovery.filename} · No file contents are stored in this
                browser.
              </p>
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => void recover()}
              >
                Check batch status
              </Button>
            </div>
          )}
          {step === 0 && (
            <>
              <label className="record-import-dropzone">
                <FileSpreadsheet aria-hidden="true" />
                <strong>{filename || "Choose a CSV or JSON file"}</strong>
                <span>Up to 15 MB · 10,000 rows per batch · UTF-8</span>
                <span>{filename ? "Change file" : "Choose file"}</span>
                <input
                  aria-label="Import file"
                  type="file"
                  accept=".csv,.json,text/csv,application/json"
                  className="sr-only"
                  disabled={busy}
                  onChange={(event) => void load(event.target.files?.[0])}
                />
              </label>
              <label className="record-import-field">
                <span>Source table name</span>
                <Input
                  value={request.source}
                  disabled={busy}
                  onChange={(event) =>
                    setRequest({ ...request, source: event.target.value })
                  }
                />
                <small>
                  Keep this name when updating the same table. Source row IDs
                  reconnect to existing records.
                </small>
              </label>
              <p className="record-import-hint">
                Company, Person, and Employment fields stay separate. Custom
                fields and JSON values come with you.
              </p>
            </>
          )}
          {step === 1 && (
            <>
              <div className="record-import-section">
                <h2>Match your columns</h2>
                <p>
                  Suggested fields are filled in. Use company.*, person.*, or
                  employment.* for custom fields. Clear a field to skip it.
                </p>
              </div>
              <datalist id="record-mapping-fields">
                {targets.map((target) => (
                  <option key={target} value={target} />
                ))}
              </datalist>
              <div className="record-import-mapping">
                <div className="record-import-mapping-heading">
                  <span>Source column</span>
                  <span />
                  <span>OpenGTM field</span>
                </div>
                {Object.entries(mapping).map(([header, target]) => (
                  <div key={header}>
                    <span title={header}>{header}</span>
                    <ArrowRight aria-hidden="true" />
                    <Input
                      aria-label={`Map ${header}`}
                      list="record-mapping-fields"
                      value={target}
                      placeholder="Skip column"
                      disabled={busy}
                      onChange={(event) =>
                        setMapping({ ...mapping, [header]: event.target.value })
                      }
                    />
                  </div>
                ))}
              </div>
            </>
          )}
          {(step === 2 || step === 3) && result && (
            <>
              <div className="record-import-result-title">
                {complete ? (
                  <CheckCircle2 aria-hidden="true" />
                ) : (
                  <FileSpreadsheet aria-hidden="true" />
                )}
                <div>
                  <h2>
                    {complete
                      ? result.replayed
                        ? "Completed batch recovered"
                        : "Your records are connected"
                      : "Ready to import?"}
                  </h2>
                  <p>
                    {filename} · {request.source}
                  </p>
                </div>
              </div>
              <dl className="record-import-totals">
                <div>
                  <dt>{complete ? "Imported" : "Accepted in preview"}</dt>
                  <dd>{result.imported.toLocaleString()}</dd>
                </div>
                <div>
                  <dt>Rejected rows</dt>
                  <dd>{result.rejected.toLocaleString()}</dd>
                </div>
                <div>
                  <dt>Total rows</dt>
                  <dd>{result.total.toLocaleString()}</dd>
                </div>
              </dl>
              <p className="record-import-hint">
                {complete
                  ? "Accepted facts are available in your record library and linked workbooks."
                  : "Accepted rows update matching identities or create records. Rejected rows are skipped. Source ownership protects manually reviewed facts."}
              </p>
              {result.warnings.map((warning) => (
                <p key={warning} className="record-import-warning">
                  {warning}
                </p>
              ))}
              {result.rejected > 0 && (
                <div
                  className="record-import-errors"
                  role="region"
                  aria-label="Rejected rows"
                  tabIndex={0}
                >
                  {result.rows
                    .filter((row) => row.error)
                    .map((row) => (
                      <p key={row.row}>
                        <strong>Row {row.row}</strong> · {row.error}
                      </p>
                    ))}
                </div>
              )}
              <Button
                variant="ghost"
                size="sm"
                onClick={() =>
                  downloadData(
                    JSON.stringify(result, null, 2),
                    "import-report.json"
                  )
                }
              >
                <Download aria-hidden="true" />
                Download row report
              </Button>
            </>
          )}
          {error && (
            <p role="alert" className="record-import-error">
              {error}
            </p>
          )}
        </div>
        <div className="record-import-footer">
          <span role="status">
            {busy ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                {step === 2 ? "Importing" : "Validating"}…{" "}
                {elapsed ? `${elapsed}s` : ""}
              </>
            ) : complete ? (
              "Import receipt saved"
            ) : (
              filename || "No file selected"
            )}
          </span>
          <div>
            {step > 0 && !complete && (
              <Button
                variant="ghost"
                disabled={busy}
                onClick={() => {
                  setError("")
                  setStep(step - 1)
                }}
              >
                <ArrowLeft />
                Back
              </Button>
            )}
            <Button variant="outline" disabled={busy} onClick={onClose}>
              {complete ? "Done" : "Cancel"}
            </Button>
            {step === 0 && (
              <Button
                disabled={busy || !filename || !request.source.trim()}
                onClick={() => void preview(1)}
              >
                Map columns
                <ArrowRight />
              </Button>
            )}
            {step === 1 && (
              <Button disabled={busy} onClick={() => void preview(2)}>
                Validate rows
                <ArrowRight />
              </Button>
            )}
            {step === 2 && (
              <Button
                disabled={busy || !result?.imported}
                onClick={() => void commit()}
              >
                Import {result?.imported.toLocaleString()} records
              </Button>
            )}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
