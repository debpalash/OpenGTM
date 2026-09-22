import { useId, useRef, useState } from "react"
import { Button, Input } from "@/design-system/primitives"
import { useSaveWorkbookColumnWidth } from "@/lib/workbook-hooks"

export function ColumnWidthEditor({ workbookId, columnId, width, onSaved }: {
  workbookId: string; columnId: string; width: number; onSaved: (width: number) => void
}) {
  const id = useId()
  const [draft, setDraft] = useState(String(width))
  const [error, setError] = useState("")
  const [saved, setSaved] = useState(false)
  const busy = useRef(false)
  const mutation = useSaveWorkbookColumnWidth(workbookId)
  return <form noValidate className="space-y-2" onSubmit={async event => {
    event.preventDefault()
    if (busy.current) return
    const value = Number(draft)
    setSaved(false)
    if (!draft.trim() || !Number.isInteger(value) || value < 80 || value > 600) {
      setError("Enter a whole number from 80 to 600.")
      return
    }
    busy.current = true
    setError("")
    try {
      const result = await mutation.mutateAsync({ columnId, width: value })
      onSaved(result.width)
      setSaved(true)
    } catch {
      setError("Width was not saved. Your value is kept; try saving again.")
    } finally { busy.current = false }
  }}>
    <label htmlFor={id} className="text-xs font-medium">Column width (pixels)</label>
    <Input id={id} type="number" min="80" max="600" step="1" value={draft}
      disabled={mutation.isPending} aria-invalid={!!error} aria-describedby={`${id}-help`}
      onChange={event => { setDraft(event.target.value); setSaved(false); setError("") }} />
    <p id={`${id}-help`} className={error ? "text-xs text-destructive" : "text-xs text-muted-foreground"}
      role={error ? "alert" : undefined}>{error || "80–600 pixels. Changes only this column’s width."}</p>
    <Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? "Saving width…" : "Save width"}</Button>
    {saved && <p role="status" className="text-xs text-muted-foreground">Column width saved.</p>}
  </form>
}
