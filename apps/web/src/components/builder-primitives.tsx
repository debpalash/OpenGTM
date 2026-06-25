// Light, shared builder primitives for the Automations rule builder slice
// (spec §4). Kept intentionally minimal — the full RuleBuilder lives in the
// Automations page (next phase). These give the slice agents reusable, a11y-ready
// pieces so the builder stays consistent:
//
//   * ConditionFieldPicker — "insert field ▾" dropdown + operator legend.
//   * ActionRow            — one ordered action row with up/down/remove controls
//                            (arrow-button reorder, fully keyboard accessible).

import type { ReactNode } from "react"
import { ChevronDown, ChevronUp, Plus, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

/** Operator legend shown beneath a condition input (backend-evaluated). */
export const CONDITION_OPERATORS = "==  !=  <  <=  >  >=  and  or"

/**
 * "Insert field ▾" dropdown populated with column names from the scoped
 * workbooks. Calls `onInsert(token)` with the field token to splice into the
 * condition string. Pair it with a static operator legend (CONDITION_OPERATORS).
 */
export function ConditionFieldPicker({
  fields,
  onInsert,
  disabled,
}: {
  fields: { id: string; name: string }[]
  onInsert: (token: string) => void
  disabled?: boolean
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 gap-1 text-xs"
            disabled={disabled || fields.length === 0}
          />
        }
      >
        <Plus className="size-3" /> insert field
        <ChevronDown className="size-3" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="max-h-64 overflow-y-auto">
        {fields.length === 0 ? (
          <DropdownMenuItem disabled>No scoped columns</DropdownMenuItem>
        ) : (
          fields.map((f) => (
            <DropdownMenuItem
              key={f.id}
              onSelect={() => onInsert(`{${f.name}}`)}
            >
              {f.name}
            </DropdownMenuItem>
          ))
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/**
 * One ordered action row with keyboard-accessible reorder + remove controls.
 * Arrow buttons (not drag) are the canonical a11y pattern (spec §8): they carry
 * aria-labels and disable at the list ends. The per-type config form is passed
 * as `children`.
 */
export function ActionRow({
  index,
  total,
  label,
  onMoveUp,
  onMoveDown,
  onRemove,
  children,
}: {
  index: number
  total: number
  label: ReactNode
  onMoveUp: () => void
  onMoveDown: () => void
  onRemove: () => void
  children?: ReactNode
}) {
  const n = index + 1
  return (
    <div className="flex flex-col gap-2 rounded-lg border p-3 md:flex-row md:items-start">
      <div className="flex min-w-0 flex-1 flex-col gap-2">
        <div className="flex items-center gap-2 text-xs font-medium">
          <span className="text-muted-foreground">{n}.</span>
          {label}
        </div>
        {children}
      </div>
      <div className="flex items-center gap-1 self-start">
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-7"
          aria-label={`Move action ${n} up`}
          aria-disabled={index === 0}
          disabled={index === 0}
          onClick={onMoveUp}
        >
          <ChevronUp className="size-3.5" />
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-7"
          aria-label={`Move action ${n} down`}
          aria-disabled={index === total - 1}
          disabled={index === total - 1}
          onClick={onMoveDown}
        >
          <ChevronDown className="size-3.5" />
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-7 text-destructive"
          aria-label={`Remove action ${n}`}
          onClick={onRemove}
        >
          <X className="size-3.5" />
        </Button>
      </div>
    </div>
  )
}
