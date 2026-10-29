import { useId, useRef, useState } from "react"
import { ArrowLeft, ArrowRight } from "lucide-react"
import { Button, Field, NativeSelect } from "@/design-system/primitives"
import { useSaveWorkbookColumnOrder } from "@/lib/workbook-hooks"
import type { ColumnConfig } from "@/lib/workbook-api"

/** Click and keyboard alternatives to dragging, using the same version check. */
export function ColumnOrderEditor({
  workbookId,
  columnId,
  columns
}: {
  workbookId: string
  columnId: string
  columns: Pick<ColumnConfig, "id" | "name">[]
}) {
  const label = useId()
  const index = columns.findIndex((column) => column.id === columnId)
  const [position, setPosition] = useState(columnId)
  const [error, setError] = useState("")
  const [receipt, setReceipt] = useState("")
  const busy = useRef(false)
  const mutation = useSaveWorkbookColumnOrder(workbookId)
  if (index < 0) return null

  async function move(targetId: string) {
    const target = columns.findIndex((column) => column.id === targetId)
    if (busy.current || target < 0 || target === index) return
    busy.current = true
    setPosition(targetId)
    setError("")
    setReceipt("")
    const columnIds = columns.map((column) => column.id)
    columnIds.splice(index, 1)
    columnIds.splice(target, 0, columnId)
    try {
      await mutation.mutateAsync({
        columnIds,
        expectedColumnIds: columns.map((column) => column.id)
      })
      setPosition(columnId)
      setReceipt(
        `Column order saved. ${columns[index].name} is now in position ${target + 1} of ${columns.length}.`
      )
    } catch (cause) {
      setError(
        `${cause instanceof Error ? cause.message : "Column order was not saved."} Your selected position is kept. Review the current order and try again.`
      )
    } finally {
      busy.current = false
    }
  }

  return (
    <section
      aria-labelledby={label}
      className="space-y-2 border-t border-b py-3"
    >
      <h2 id={label} className="text-sm font-medium">
        Column order
      </h2>
      <p className="text-xs text-muted-foreground">
        Position {index + 1} of {columns.length}, including hidden columns.
        Record identity stays pinned.
      </p>
      <div className="flex gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={index === 0 || mutation.isPending}
          onClick={() => void move(columns[index - 1].id)}
          startIcon={<ArrowLeft aria-hidden="true" className="size-3.5" />}
        >
          Move left
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={index === columns.length - 1 || mutation.isPending}
          onClick={() => void move(columns[index + 1].id)}
          endIcon={<ArrowRight aria-hidden="true" className="size-3.5" />}
        >
          Move right
        </Button>
      </div>
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault()
          void move(position)
        }}
        className="space-y-2"
      >
        <Field
          label="Column position"
          description="Choose any position, then save the order."
          error={error || undefined}
        >
          {(control) => (
            <NativeSelect
              {...control}
              value={position}
              disabled={mutation.isPending}
              onChange={(event) => {
                setPosition(event.target.value)
                setError("")
                setReceipt("")
              }}
            >
              {columns.map((column, positionIndex) => (
                <option key={column.id} value={column.id}>
                  {positionIndex + 1} · {column.name}
                </option>
              ))}
            </NativeSelect>
          )}
        </Field>
        <Button
          type="submit"
          variant="solid"
          loading={mutation.isPending}
          disabled={position === columnId}
        >
          Save position
        </Button>
      </form>
      {receipt && (
        <p role="status" className="text-xs text-muted-foreground">
          {receipt}
        </p>
      )}
    </section>
  )
}
