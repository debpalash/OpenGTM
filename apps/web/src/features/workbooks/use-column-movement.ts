import { useEffect, useRef, useState } from "react"
import {
  KeyboardSensor,
  PointerSensor,
  useSensor,
  useSensors,
  type DndContextProps
} from "@dnd-kit/core"
import { toast } from "sonner"
import { useSaveWorkbookColumnOrder } from "@/lib/workbook-hooks"
import type { ColumnConfig } from "@/lib/workbook-api"

/** Pointer collision and keyboard intent share one version-checked save path.
 * Keyboard targets update synchronously: an immediate Space/drop must not read
 * the previous frame's collision result, nor lose repeated arrow presses.
 */
export function useColumnMovement(
  workbookId: string,
  columns: ColumnConfig[],
  hidden: Set<string>
): Pick<
  DndContextProps,
  "sensors" | "onDragStart" | "onDragEnd" | "onDragCancel" | "accessibility"
> {
  const keyboardTarget = useRef<string | null>(null)
  const [restoreFocus, setRestoreFocus] = useState<string | null>(null)
  const save = useSaveWorkbookColumnOrder(workbookId)
  const visible = columns.filter((column) => !hidden.has(column.id))
  const name = (id: string | number) =>
    columns.find((column) => column.id === id)?.name || "Unknown"
  useEffect(() => {
    if (!restoreFocus) return
    const frame = requestAnimationFrame(() => {
      // Refetching the saved order replaces header renderers. Focus the current
      // handle after that commit; do not interrupt another control the user chose.
      const header = Array.from(
        document.querySelectorAll<HTMLElement>("thead th[data-col-id]")
      ).find((element) => element.dataset.colId === restoreFocus)
      const handle = header?.querySelector<HTMLButtonElement>(
        ".workbook-column-drag"
      )
      if (
        document.activeElement === document.body ||
        document.activeElement === handle
      )
        handle?.focus({ preventScroll: true })
      setRestoreFocus(null)
    })
    return () => cancelAnimationFrame(frame)
  }, [restoreFocus, columns])
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
    useSensor(KeyboardSensor, {
      coordinateGetter: (event, args) => {
        if (event.code !== "ArrowLeft" && event.code !== "ArrowRight")
          return undefined
        event.preventDefault()
        const { over, collisionRect, droppableRects } = args.context
        const index = visible.findIndex(
          (column) =>
            column.id === (keyboardTarget.current ?? over?.id ?? args.active)
        )
        const target = visible[index + (event.code === "ArrowRight" ? 1 : -1)]
        if (index < 0 || !target) return undefined
        keyboardTarget.current = target.id
        // Measurement may still be refreshing immediately after resize. Keep
        // logical intent even when that frame cannot provide a drag preview.
        const rect = droppableRects.get(target.id)
        if (!rect || !collisionRect) return undefined
        // Center-align unequal widths; edge alignment can retain the old target.
        return {
          x: rect.left + (rect.width - collisionRect.width) / 2,
          y: collisionRect.top
        }
      }
    })
  )
  return {
    sensors,
    onDragStart: ({ active, activatorEvent }) => {
      setRestoreFocus(null)
      keyboardTarget.current =
        activatorEvent.type === "keydown" ? String(active.id) : null
    },
    onDragCancel: ({ active }) => {
      if (keyboardTarget.current !== null) setRestoreFocus(String(active.id))
      keyboardTarget.current = null
    },
    onDragEnd: ({ active, over }) => {
      const focusId = keyboardTarget.current !== null ? String(active.id) : null
      const targetId = keyboardTarget.current ?? over?.id
      if (!targetId || active.id === targetId) return
      if (save.isPending) {
        setRestoreFocus(focusId)
        toast.error(
          "Column order is still saving. Move the column again after confirmation."
        )
        return
      }
      const before = columns.map((column) => column.id)
      const oldIndex = before.indexOf(String(active.id))
      const newIndex = before.indexOf(String(targetId))
      if (oldIndex < 0 || newIndex < 0) return
      const columnIds = [...before]
      columnIds.splice(oldIndex, 1)
      columnIds.splice(newIndex, 0, String(active.id))
      save.mutate(
        { columnIds, expectedColumnIds: before },
        {
          onSuccess: () => {
            setRestoreFocus(focusId)
            toast.success(
              `${name(active.id)} column order saved at position ${newIndex + 1}.`
            )
          },
          onError: (error) => {
            setRestoreFocus(focusId)
            toast.error(
              error.message ||
                "Column order was not saved. Refresh before retrying."
            )
          }
        }
      )
    },
    accessibility: {
      screenReaderInstructions: {
        draggable:
          "Press Space to pick up the column, Left or Right to move, Space to save its position, or Escape to cancel. Column settings also offer Move left, Move right and a position picker."
      },
      announcements: {
        onDragStart: ({ active }) => `Picked up ${name(active.id)} column.`,
        onDragOver: ({ active, over }) => {
          const target = keyboardTarget.current ?? over?.id
          const index = visible.findIndex((column) => column.id === target)
          return index < 0
            ? undefined
            : `${name(active.id)} column at position ${index + 1} of ${visible.length} visible columns.`
        },
        onDragEnd: () =>
          "Column movement ended. Any changed position is being saved.",
        onDragCancel: ({ active }) =>
          `${name(active.id)} column movement canceled. Its position is unchanged.`
      }
    }
  }
}
