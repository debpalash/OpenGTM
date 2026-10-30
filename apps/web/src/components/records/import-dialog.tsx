import { useId, useLayoutEffect, useRef } from "react"
import {
  ArrowRight,
  ArrowLeft,
  CheckCircle2,
  FileSpreadsheet,
  Loader2
} from "lucide-react"
import {
  Button,
  Input,
  Modal,
  ModalContent,
  ModalHeader,
  ModalTitle,
  ModalDescription
} from "@/design-system/primitives"
import type { RecordKind } from "@/lib/records"
import { ColumnMapping } from "@/features/records/import/column-mapping"
import { ImportReview } from "@/features/records/import/import-review"
import { useImportTask } from "@/features/records/import/use-import-task"
import "./import-dialog.css"
import "@/features/records/import/import.css"

export function RecordImportDialog({
  kind,
  onClose,
  onImported,
  onOpenRecord,
  onSelectImported
}: {
  kind: RecordKind
  onClose: () => void
  onImported: () => void
  onOpenRecord: (id: string) => void
  onSelectImported: (ids: string[]) => void
}) {
  const task = useImportTask(kind, onImported, onClose)
  const id = useId()
  const title = useRef<HTMLHeadingElement>(null),
    previousStep = useRef(task.step)
  useLayoutEffect(() => {
    if (previousStep.current !== task.step)
      title.current?.focus({ preventScroll: true })
    previousStep.current = task.step
  }, [task.step])
  const steps = ["Choose file", "Map columns", "Review", "Results"]
  return (
    <Modal
      open
      onOpenChange={(open, details) => {
        if (!open) {
          details.cancel()
          void task.close()
        }
      }}
    >
      <ModalContent
        className="record-import-dialog"
        showCloseButton={!task.busy}
      >
        <ModalHeader>
          <ModalTitle ref={title} tabIndex={-1}>
            Import {kind === "company" ? "companies" : "people"} ·{" "}
            {steps[task.step]}
          </ModalTitle>
          <ModalDescription>
            CSV or JSON into your shared record library.
          </ModalDescription>
        </ModalHeader>
        <ol className="record-import-steps" aria-label="Import progress">
          {steps.map((label, index) => (
            <li
              key={label}
              aria-current={task.step === index ? "step" : undefined}
              data-complete={task.step > index}
            >
              <span>
                {task.step > index ? (
                  <CheckCircle2 aria-hidden="true" />
                ) : (
                  index + 1
                )}
              </span>
              {label}
            </li>
          ))}
        </ol>
        <div className="record-import-body" aria-busy={task.busy}>
          {task.recovery && !task.complete && (
            <div className="record-import-recovery">
              <strong>Check your last batch</strong>
              <p>
                {task.recovery.filename} · No file contents are stored in this
                browser.
              </p>
              <p>
                Keep its source and mapping unchanged while the outcome is
                unknown.
              </p>
              <Button
                size="sm"
                variant="outline"
                disabled={task.busy}
                onClick={() => void task.recover()}
              >
                Check batch status
              </Button>
              <Button
                size="sm"
                variant="ghost"
                disabled={task.busy}
                onClick={() => void task.startAnother()}
              >
                Start another batch
              </Button>
            </div>
          )}
          {task.step === 0 && (
            <>
              <label className="record-import-dropzone">
                <FileSpreadsheet aria-hidden="true" />
                <strong>{task.filename || "Choose a CSV or JSON file"}</strong>
                <span>
                  Up to 15 MB · 10,000 rows · 256 columns per row · UTF-8
                </span>
                <span>{task.filename ? "Change file" : "Choose file"}</span>
                <input
                  aria-label="Import file"
                  type="file"
                  accept=".csv,.json,text/csv,application/json"
                  className="sr-only"
                  disabled={task.busy}
                  onChange={(event) => {
                    void task.load(event.target.files?.[0])
                    event.target.value = ""
                  }}
                />
              </label>
              <div className="record-import-field">
                <label htmlFor={`${id}-source`}>Source table name</label>
                <Input
                  id={`${id}-source`}
                  aria-describedby={`${id}-source-help`}
                  value={task.request.source}
                  maxLength={200}
                  disabled={task.busy || task.locked}
                  onChange={(event) => task.setSource(event.target.value)}
                />
                <small id={`${id}-source-help`}>
                  Keep this name for updates to the same source table. Source
                  row IDs reconnect to existing records.
                </small>
              </div>
            </>
          )}
          {task.step === 1 && task.inspection && (
            <ColumnMapping
              inspection={task.inspection}
              mapping={task.mapping}
              kind={kind}
              disabled={task.busy || task.locked}
              onChange={task.setTarget}
            />
          )}
          {(task.step === 2 || task.complete) && task.result && (
            <ImportReview
              result={task.result}
              complete={task.complete}
              filename={task.filename}
              source={task.request.source}
              kind={kind}
              onOpenRecord={onOpenRecord}
              onSelectImported={onSelectImported}
            />
          )}
          {task.storageError && (
            <p role="status" className="record-import-warning">
              {task.storageError}
            </p>
          )}
          {task.error && (
            <p role="alert" className="record-import-error">
              {task.error}
            </p>
          )}
        </div>
        <div className="record-import-footer">
          <span role="status">
            {task.busy ? (
              <>
                <Loader2 aria-hidden="true" className="animate-spin" />
                {
                  {
                    reading: "Reading file",
                    inspecting: "Inspecting columns",
                    validating: "Validating rows",
                    importing: "Importing",
                    recovering: "Checking batch",
                    ready: ""
                  }[task.phase]
                }
                … {task.elapsed ? `${task.elapsed}s` : ""}
              </>
            ) : task.complete ? (
              "Import confirmed"
            ) : (
              task.filename || "No file selected"
            )}
          </span>
          <div>
            {task.step > 0 && !task.complete && (
              <Button variant="ghost" disabled={task.busy} onClick={task.back}>
                <ArrowLeft aria-hidden="true" />
                Back
              </Button>
            )}
            <Button
              variant="outline"
              disabled={task.busy}
              onClick={() => void task.close()}
            >
              {task.complete ? "Done" : "Cancel"}
            </Button>
            {task.step === 0 && (
              <Button
                disabled={
                  task.busy || !task.filename || !task.request.source.trim()
                }
                onClick={() => void task.inspect()}
              >
                Map columns
                <ArrowRight aria-hidden="true" />
              </Button>
            )}
            {task.step === 1 && (
              <Button
                disabled={task.busy || task.issueCount > 0}
                onClick={() => void task.preview()}
              >
                Validate rows
                <ArrowRight aria-hidden="true" />
              </Button>
            )}
            {task.step === 2 && (
              <Button
                disabled={task.busy || !task.result?.imported}
                onClick={() => void task.commit()}
              >
                Import {task.result?.imported.toLocaleString()}{" "}
                {task.result?.imported === 1 ? "row" : "rows"}
              </Button>
            )}
          </div>
        </div>
      </ModalContent>
    </Modal>
  )
}
