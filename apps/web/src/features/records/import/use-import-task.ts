import { useEffect, useRef, useState } from "react"
import { getActiveWorkspace, getToken } from "@/lib/auth"
import {
  inspectRecordImport,
  recordImportAPI,
  fetchImportReceipt,
  ImportRequestError,
  type ImportInspection
} from "@/lib/record-import"
import type { ImportResult, RecordImport, RecordKind } from "@/lib/records"
import {
  useLeaveConfirmation,
  useLeaveGuard
} from "@/features/navigation/leave-guard-provider"
import { mappingIssues } from "./mapping"
import {
  readImportDraft,
  encodeImportDraft,
  type ImportDraft
} from "./recovery"

export function useImportTask(
  kind: RecordKind,
  onImported: () => void,
  onClose: () => void
) {
  const [scope] = useState(() => ({
    workspace: getActiveWorkspace(),
    token: getToken()
  }))
  const storageKey = `opengtm-import:${scope.workspace}:${kind}`
  const [recovery, setRecovery] = useState(() => {
    try {
      return readImportDraft(sessionStorage.getItem(storageKey))
    } catch {
      return null
    }
  })
  const [request, setRequest] = useState<RecordImport>(() => ({
    record_type: kind,
    source: recovery?.source || "clay:prospects",
    idempotency_key: recovery?.key || crypto.randomUUID()
  }))
  const [rawRows, setRawRows] = useState("")
  const [filename, setFilename] = useState("")
  const [hash, setHash] = useState("")
  const [inspection, setInspection] = useState<ImportInspection | null>(null)
  const [result, setResult] = useState<ImportResult | null>(null)
  const [mapping, setMapping] = useState<Record<string, string>>({})
  const [step, setStep] = useState(0)
  const [phase, setPhase] = useState<
    | "ready"
    | "reading"
    | "inspecting"
    | "validating"
    | "importing"
    | "recovering"
  >("ready")
  const [elapsed, setElapsed] = useState(0)
  const [error, setError] = useState("")
  const [storageError, setStorageError] = useState("")
  const pending = useRef(false),
    alive = useRef(true)
  const leave = useLeaveConfirmation()
  const complete = step === 3,
    busy = phase !== "ready"
  const release = useLeaveGuard({
    dirty: !complete && (!!filename || !!recovery),
    busy,
    busyLabel:
      phase === "importing"
        ? "Import in progress"
        : "File validation in progress",
    message: recovery
      ? "This batch may already have imported. Discarding its reminder does not undo it, and a new batch may duplicate records. Keep editing to check batch status with the same import key."
      : "Your file and column mapping will be discarded. Keep editing to finish the import."
  })
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  useEffect(() => {
    if (!busy) return
    const started = Date.now()
    const timer = setInterval(
      () => setElapsed(Math.floor((Date.now() - started) / 1000)),
      1000
    )
    return () => clearInterval(timer)
  }, [busy])
  function assertScope() {
    if (scope.workspace !== getActiveWorkspace() || scope.token !== getToken())
      throw new Error(
        "Your workspace or session changed. Reopen import in the current workspace before continuing."
      )
  }
  async function perform(next: typeof phase, work: () => Promise<void>) {
    if (pending.current) return
    pending.current = true
    setPhase(next)
    setElapsed(0)
    setError("")
    try {
      assertScope()
      await work()
    } catch (cause) {
      if (alive.current)
        setError(
          cause instanceof Error
            ? cause.message
            : "The request could not be confirmed. Your selections are kept."
        )
    } finally {
      pending.current = false
      if (alive.current) setPhase("ready")
    }
  }
  function forget() {
    setRecovery(null)
    try {
      sessionStorage.removeItem(storageKey)
    } catch {
      setStorageError(
        "Browser storage is unavailable. A previous recovery reminder may remain after reload; checking it is safe."
      )
    }
  }
  async function close() {
    if (pending.current) return
    if (await leave.request("Close import")) {
      release()
      onClose()
    }
  }
  async function load(file?: File) {
    if (!file) return
    await perform("reading", async () => {
      if (file.size > 15 * 1024 * 1024)
        throw new Error(
          "Use files up to 15 MB and 10,000 rows. Split larger exports into batches."
        )
      const text = await file.text()
      const json = file.name.toLowerCase().endsWith(".json")
      let rows: Record<string, unknown>[] | undefined
      if (json) {
        const value: unknown = JSON.parse(text)
        if (
          !Array.isArray(value) ||
          value.some(
            (row) => !row || typeof row !== "object" || Array.isArray(row)
          )
        )
          throw new Error("JSON must be an array of row objects.")
        rows = value
      }
      const digest = await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(text)
      )
      const fingerprint = Array.from(new Uint8Array(digest), (v) =>
        v.toString(16).padStart(2, "0")
      ).join("")
      if (recovery && recovery.hash !== fingerprint)
        throw new Error(
          `An earlier batch is unresolved. Reselect ${recovery.filename}, or check its batch status before starting another file.`
        )
      if (!alive.current) return
      assertScope()
      setHash(fingerprint)
      setRawRows(json ? text : "")
      setFilename(file.name)
      setMapping(recovery?.mapping || {})
      setInspection(null)
      setResult(null)
      setStep(0)
      setRequest({
        record_type: kind,
        source: recovery?.source || request.source,
        idempotency_key: recovery?.key || crypto.randomUUID(),
        ...(json ? { rows } : { csv: text })
      })
    })
  }
  async function inspect() {
    await perform("inspecting", async () => {
      const output = await inspectRecordImport({ ...request, mapping }, rawRows)
      if (!alive.current) return
      assertScope()
      setInspection(output)
      setMapping(
        Object.fromEntries(
          output.columns.map((column) => [column.name, column.target])
        )
      )
      setStep(1)
    })
  }
  async function preview() {
    if (!inspection || mappingIssues(inspection.columns, mapping, kind).size)
      return
    await perform("validating", async () => {
      const output = await recordImportAPI(
        { ...request, mapping },
        true,
        rawRows
      )
      if (!alive.current) return
      assertScope()
      setResult(output)
      setStep(2)
    })
  }
  async function commit() {
    if (!result?.preview || !result.imported || step !== 2) return
    await perform("importing", async () => {
      const draft: ImportDraft = {
        key: request.idempotency_key,
        hash,
        filename,
        source: request.source,
        mapping
      }
      setRecovery(draft)
      try {
        sessionStorage.setItem(storageKey, encodeImportDraft(draft))
        setStorageError("")
      } catch {
        setStorageError(
          "Reload recovery could not be saved in this browser. Keep this tab open until the import is confirmed."
        )
      }
      let output: ImportResult
      try {
        output = await recordImportAPI({ ...request, mapping }, false, rawRows)
      } catch (cause) {
        if (cause instanceof ImportRequestError && cause.outcome === "refused")
          forget()
        throw new Error(
          `${cause instanceof Error ? cause.message : "The response was lost."} Check batch status before retrying. This batch keeps the same import key.`
        )
      }
      if (!alive.current) return
      assertScope()
      setResult(output)
      setStep(3)
      setStorageError("")
      forget()
      onImported()
    })
  }
  async function recover() {
    if (!recovery) return
    await perform("recovering", async () => {
      const output = await fetchImportReceipt(recovery.key)
      if (!alive.current) return
      assertScope()
      setFilename(recovery.filename)
      setRequest((previous) => ({
        ...previous,
        source: recovery.source,
        idempotency_key: recovery.key
      }))
      setResult(output)
      setStep(3)
      setStorageError("")
      forget()
      onImported()
    })
  }
  async function startAnother() {
    if (pending.current || !(await leave.request("Start another batch"))) return
    forget()
    setRequest({
      record_type: kind,
      source: request.source,
      idempotency_key: crypto.randomUUID()
    })
    setRawRows("")
    setFilename("")
    setHash("")
    setInspection(null)
    setResult(null)
    setMapping({})
    setStep(0)
    setError("")
  }
  return {
    request,
    filename,
    inspection,
    result,
    mapping,
    recovery,
    step,
    busy,
    phase,
    elapsed,
    error,
    storageError,
    complete,
    locked: !!recovery,
    issueCount: inspection
      ? mappingIssues(inspection.columns, mapping, kind).size
      : 0,
    load,
    inspect,
    preview,
    commit,
    recover,
    close,
    startAnother,
    setSource: (source: string) => {
      if (!busy && !recovery) {
        setRequest((previous) => ({ ...previous, source }))
        setResult(null)
      }
    },
    setTarget: (name: string, target: string) => {
      if (!busy && !recovery) {
        setMapping((previous) => ({ ...previous, [name]: target }))
        setResult(null)
      }
    },
    back: () => {
      if (!busy && step > 0 && !complete) {
        setStep(step - 1)
        setError("")
      }
    }
  }
}
