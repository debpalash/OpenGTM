import { useRef, type RefObject } from "react"
import { AlertDialog, Button } from "@/design-system/primitives"
import type { ColumnConfig } from "@/lib/workbook-api"
import { useDeleteWorkbookColumn } from "@/lib/workbook-hooks"
import "./selection-bar.css"

export function DeleteColumnDialog({ workbookId, column, onClose, finalFocus }: {
  workbookId: string; column: ColumnConfig; onClose: () => void
  finalFocus: RefObject<HTMLDivElement | null>
}) {
  const deletion = useDeleteWorkbookColumn(workbookId)
  const submitting = useRef(false)
  async function remove() {
    if (submitting.current || deletion.isError) return
    submitting.current = true
    try { await deletion.mutateAsync(column); onClose() }
    catch { /* Keep the confirmation visible; require review after failure. */ }
    finally { submitting.current = false }
  }
  return <AlertDialog.Root open onOpenChange={(open, details) => {
    if (submitting.current) { details.cancel(); return }
    if (!open) onClose()
  }}>
    <AlertDialog.Popup className="gtm-selection-dialog" finalFocus={() => finalFocus.current}>
      <AlertDialog.Header>
        <AlertDialog.Title>Delete {column.name} column?</AlertDialog.Title>
        <AlertDialog.Description>Removes this column definition and permanently deletes its legacy enrichment records. Stored workbook row data and evidence, and original leads, are retained. This is not complete data erasure.</AlertDialog.Description>
      </AlertDialog.Header>
      <AlertDialog.Body>
        <p className="text-sm">Other columns are unchanged. References to this column may need updating. Existing external actions are not undone.</p>
        {deletion.isError && <p role="alert" className="mt-3 text-sm text-destructive">{deletion.error.message} Cancel and refresh the workbook before confirming again.</p>}
      </AlertDialog.Body>
      <AlertDialog.Footer>
        <AlertDialog.Close render={<Button disabled={deletion.isPending} />}>Cancel</AlertDialog.Close>
        <Button color="danger" variant="solid" disabled={deletion.isPending || deletion.isError} onClick={() => void remove()}>{deletion.isPending ? "Deleting…" : "Delete column"}</Button>
      </AlertDialog.Footer>
    </AlertDialog.Popup>
  </AlertDialog.Root>
}
