import { useId, type ReactNode } from "react"
import { cn } from "@/lib/utils"

export interface FieldControlProps {
  id: string
  "aria-describedby"?: string
  "aria-invalid"?: true
  required?: boolean
}

/** One label/help/error contract for native and application-owned controls. */
export function Field({
  id,
  label,
  description,
  error,
  required,
  className,
  children
}: {
  id?: string
  label: ReactNode
  description?: ReactNode
  error?: string
  required?: boolean
  className?: string
  children: (control: FieldControlProps) => ReactNode
}) {
  const generated = useId()
  const controlId = id ?? `field-${generated}`
  const describedBy =
    [description ? `${controlId}-help` : "", error ? `${controlId}-error` : ""]
      .filter(Boolean)
      .join(" ") || undefined
  return (
    <div className={cn("gtm-field", className)} data-invalid={!!error}>
      <label htmlFor={controlId} className="gtm-field-label">
        {label}
        {required && <span className="gtm-field-required"> (required)</span>}
      </label>
      {children({
        id: controlId,
        "aria-describedby": describedBy,
        "aria-invalid": error ? true : undefined,
        required: required || undefined
      })}
      {description && (
        <p id={`${controlId}-help`} className="gtm-field-help">
          {description}
        </p>
      )}
      {error && (
        <p id={`${controlId}-error`} className="gtm-field-error" role="alert">
          {error}
        </p>
      )}
    </div>
  )
}
