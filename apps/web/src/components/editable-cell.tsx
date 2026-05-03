import { useState, useRef, useEffect } from "react"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"

interface EditableCellProps {
  value: string
  onSave: (value: string) => void
  className?: string
  placeholder?: string
}

export function EditableCell({ value, onSave, className, placeholder = "—" }: EditableCellProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(value)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (editing && inputRef.current) {
      inputRef.current.focus()
      inputRef.current.select()
    }
  }, [editing])

  // Sync external value changes
  useEffect(() => {
    if (!editing) setDraft(value)
  }, [value, editing])

  const commit = () => {
    setEditing(false)
    if (draft.trim() !== value) {
      onSave(draft.trim())
    }
  }

  const cancel = () => {
    setDraft(value)
    setEditing(false)
  }

  if (editing) {
    return (
      <Input
        ref={inputRef}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === "Enter") commit()
          if (e.key === "Escape") cancel()
        }}
        onClick={(e) => e.stopPropagation()}
        className={cn("h-7 text-sm px-1.5 py-0", className)}
      />
    )
  }

  return (
    <span
      className={cn(
        "cursor-text rounded px-1 py-0.5 text-sm hover:bg-muted/50 transition-colors",
        !value && "text-muted-foreground italic",
        className,
      )}
      onClick={(e) => {
        e.stopPropagation()
        setEditing(true)
      }}
      title="Click to edit"
    >
      {value || placeholder}
    </span>
  )
}
