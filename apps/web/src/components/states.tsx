// Shared loading / empty / error view states (spec §7). Every list/detail view
// renders all three. Densities match the existing pages (outreach/signals).

import type { ComponentType, ReactNode } from "react"
import { AlertCircle, Inbox, Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { apiErrorMessage } from "@/lib/automation-hooks"

/** Inline loading state. `rows` renders skeleton bars; otherwise a spinner. */
export function Loading({ rows, label }: { rows?: number; label?: string }) {
  if (rows && rows > 0) {
    return (
      <div className="flex flex-col gap-2 p-1">
        {Array.from({ length: rows }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full rounded-md" />
        ))}
      </div>
    )
  }
  return (
    <div className="flex items-center justify-center gap-2 p-8 text-xs text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      {label ?? "Loading…"}
    </div>
  )
}

/** Empty-state block with icon + copy + optional primary CTA. */
export function Empty({
  icon: Icon = Inbox,
  title,
  description,
  action,
}: {
  icon?: ComponentType<{ className?: string }>
  title: string
  description?: string
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 p-10 text-center">
      <div className="flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <Icon className="size-4" />
      </div>
      <p className="text-sm font-medium">{title}</p>
      {description && (
        <p className="max-w-sm text-xs text-muted-foreground">{description}</p>
      )}
      {action && <div className="mt-1">{action}</div>}
    </div>
  )
}

/** Inline error row with retry. Maps ApiError → friendly message. */
export function ErrorState({
  error,
  onRetry,
}: {
  error: unknown
  onRetry?: () => void
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 p-8 text-center">
      <AlertCircle className="size-5 text-destructive" />
      <p className="text-sm font-medium">Something went wrong</p>
      <p className="max-w-sm text-xs text-muted-foreground">
        {apiErrorMessage(error)}
      </p>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry} className="mt-1">
          Retry
        </Button>
      )}
    </div>
  )
}
