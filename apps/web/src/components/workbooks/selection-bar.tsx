import { useRef, useState, type RefObject } from "react"
import { Download, Trash2, X } from "lucide-react"
import { AlertDialog, Button } from "@/design-system/primitives"
import type { WorkbookDeleteScope } from "@/lib/workbook-selection"
import "./selection-bar.css"

export function WorkbookSelectionBar({ count, visibleCount, matchingCount, allMatching, busy, exportDisabled, onSelectAll, onClear, onExport, getDeleteScope, onDelete, finalFocus }: {
  count: number; visibleCount: number; matchingCount: number; allMatching: boolean
  busy: boolean; exportDisabled: boolean
  onSelectAll: () => void; onClear: () => void; onExport: () => void
  getDeleteScope: () => WorkbookDeleteScope
  onDelete: (scope: WorkbookDeleteScope) => Promise<void>
  finalFocus: RefObject<HTMLDivElement | null>
}) {
  const [scope, setScope] = useState<WorkbookDeleteScope | null>(null)
  const [error, setError] = useState("")
  const [pending, setPending] = useState(false)
  const submitting = useRef(false)
  const trigger = useRef<HTMLButtonElement>(null)
  const selected = allMatching ? matchingCount : count
  const deleteCount = scope?.kind === "matching" ? scope.count : scope?.rowIds.length ?? 0

  async function remove() {
    if (!scope || submitting.current) return
    submitting.current = true
    setPending(true)
    setError("")
    try { await onDelete(scope); setScope(null) }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Could not delete rows. Try again.") }
    finally { submitting.current = false; setPending(false) }
  }

  return <>
    {selected > 0 && <section aria-label="Selected workbook rows" className="gtm-selection-bar">
      <p role="status">{allMatching ? `${selected} matching rows selected across all pages` : `${count} rows selected · ${visibleCount} on this page`}</p>
      {!allMatching && matchingCount > count && <Button variant="ghost" disabled={busy} onClick={onSelectAll}>Select all {matchingCount} matching rows</Button>}
      <Button disabled={busy || exportDisabled} onClick={onExport} startIcon={<Download aria-hidden="true" className="size-4" />}>Export selected</Button>
      <Button ref={trigger} color="danger" disabled={busy} onClick={() => {
        setError("")
        try { setScope(getDeleteScope()) }
        catch (cause) { setError(cause instanceof Error ? cause.message : "Invalid selection") }
      }} startIcon={<Trash2 aria-hidden="true" className="size-4" />}>Delete selected</Button>
      <Button variant="ghost" disabled={busy} onClick={() => { setError(""); onClear() }} startIcon={<X aria-hidden="true" className="size-4" />}>Clear selection</Button>
      {error && !scope && <p role="alert" className="text-destructive">{error}</p>}
    </section>}
    <AlertDialog.Root open={!!scope} onOpenChange={(open, details) => {
      if (submitting.current) { details.cancel(); return }
      if (!open) { setScope(null); setError("") }
    }}>
      <AlertDialog.Popup className="gtm-selection-dialog" finalFocus={() => trigger.current?.isConnected ? trigger.current : finalFocus.current}>
        <AlertDialog.Header><AlertDialog.Title>Delete {deleteCount} workbook rows?</AlertDialog.Title>
        <AlertDialog.Description>
          {scope?.kind === "matching" ? "This applies to every row matching the current search and saved view, across all pages." : "Only the selected workbook row IDs will be deleted, including selections on other pages."}
          {" "}Original leads are unchanged. This cannot be undone.
        </AlertDialog.Description></AlertDialog.Header>
        {(scope?.kind === "matching" || error) && <AlertDialog.Body>
          {scope?.kind === "matching" && <p className="text-sm">Search: {scope.search || "None"} · Saved view: {scope.viewId ? "Current saved view" : "All rows"}</p>}
          {error && <p role="alert" className="mt-3 text-sm text-destructive">{error}</p>}
        </AlertDialog.Body>}
        <AlertDialog.Footer><AlertDialog.Close render={<Button disabled={pending} />}>Cancel</AlertDialog.Close><Button color="danger" variant="solid" loading={pending} onClick={() => void remove()}>Delete {deleteCount} rows</Button></AlertDialog.Footer>
      </AlertDialog.Popup>
    </AlertDialog.Root>
  </>
}
