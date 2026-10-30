import { useLayoutEffect, useRef, useState } from "react"
import { ArrowDown, ArrowUp, Columns3, LockKeyhole } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import type { RecordKind } from "@/lib/records"
import {
  defaultColumnLayout,
  moveColumn,
  recordColumns,
  visibleColumns,
  type ColumnLayout
} from "./columns"

export function ColumnConfiguration({
  kind,
  layout,
  temporary,
  onApply
}: {
  kind: RecordKind
  layout: ColumnLayout
  temporary: boolean
  onApply: (layout: ColumnLayout) => void
}) {
  const [open, setOpen] = useState(false)
  const trigger = useRef<HTMLButtonElement>(null)
  return (
    <>
      <Button
        ref={trigger}
        variant="outline"
        size="sm"
        onClick={() => setOpen(true)}
        aria-haspopup="dialog"
        aria-expanded={open}
      >
        <Columns3 aria-hidden="true" /> Columns
      </Button>
      {open && (
        <ColumnDialog
          kind={kind}
          initial={layout}
          temporary={temporary}
          trigger={trigger}
          close={() => setOpen(false)}
          apply={onApply}
        />
      )}
    </>
  )
}
function ColumnDialog({
  kind,
  initial,
  temporary,
  trigger,
  close,
  apply
}: {
  kind: RecordKind
  initial: ColumnLayout
  temporary: boolean
  trigger: React.RefObject<HTMLButtonElement | null>
  close: () => void
  apply: (layout: ColumnLayout) => void
}) {
  const [draft, setDraft] = useState(initial)
  const [announcement, setAnnouncement] = useState("")
  const inputs = useRef(new Map<string, HTMLInputElement>())
  const moved = useRef<string | null>(null)
  const columns = recordColumns(kind)
  useLayoutEffect(() => {
    if (moved.current) {
      inputs.current.get(moved.current)?.focus()
      moved.current = null
    }
  }, [draft])
  const move = (id: string, direction: -1 | 1) => {
    const next = moveColumn(draft, id, direction)
    if (next === draft) return
    moved.current = id
    setDraft(next)
    setAnnouncement(
      `${columns.find((column) => column.id === id)!.label} moved to position ${next.order.indexOf(id) + 1}.`
    )
  }
  return (
    <Dialog
      open
      onOpenChange={(isOpen) => {
        if (!isOpen) close()
      }}
    >
      <DialogContent className="record-columns-dialog" finalFocus={trigger}>
        <DialogHeader>
          <DialogTitle>
            {kind === "company" ? "Company" : "People"} columns
          </DialogTitle>
          <DialogDescription>
            Choose and order columns in this workspace.{" "}
            {kind === "company" ? "Company" : "Person"} identity stays pinned.
            Applies to your browser; filters and selected records stay
            unchanged.
          </DialogDescription>
        </DialogHeader>
        <div className="record-columns-scroll">
          <p className="record-columns-meta">
            {visibleColumns(kind, draft).length} of {columns.length} columns
            shown. Hidden columns keep their position.
          </p>
          {temporary && (
            <p className="record-columns-notice">
              This browser could not store the preference. Changes will apply to
              this view until you leave.
            </p>
          )}
          <ol className="record-columns-list" aria-label="Column order">
            {draft.order.map((id, index) => {
              const column = columns.find((item) => item.id === id)!
              const pinned = id === "identity"
              return (
                <li key={id}>
                  <label className="record-column-choice">
                    <input
                      type="checkbox"
                      checked={pinned || !draft.hidden.includes(id)}
                      disabled={pinned}
                      ref={(input) => {
                        if (input) inputs.current.set(id, input)
                        else inputs.current.delete(id)
                      }}
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          hidden: event.target.checked
                            ? current.hidden.filter((key) => key !== id)
                            : [...current.hidden, id]
                        }))
                      }
                    />
                    <span>
                      {column.label}
                      {pinned && (
                        <span className="record-columns-pinned">
                          <LockKeyhole aria-hidden="true" /> Pinned
                        </span>
                      )}
                    </span>
                  </label>
                  {!pinned && (
                    <div className="record-column-move">
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label={`Move ${column.label} up`}
                        disabled={index === 1}
                        onClick={() => move(id, -1)}
                      >
                        <ArrowUp aria-hidden="true" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label={`Move ${column.label} down`}
                        disabled={index === draft.order.length - 1}
                        onClick={() => move(id, 1)}
                      >
                        <ArrowDown aria-hidden="true" />
                      </Button>
                    </div>
                  )}
                </li>
              )
            })}
          </ol>
          <p className="sr-only" role="status">
            {announcement}
          </p>
        </div>
        <DialogFooter>
          <Button
            variant="ghost"
            onClick={() => {
              setDraft(defaultColumnLayout(kind))
              setAnnouncement(
                "Default columns restored in this preview. Apply columns to save."
              )
            }}
          >
            Reset to default
          </Button>
          <Button variant="outline" onClick={close}>
            Cancel
          </Button>
          <Button
            onClick={() => {
              apply(draft)
              close()
            }}
          >
            Apply columns
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
