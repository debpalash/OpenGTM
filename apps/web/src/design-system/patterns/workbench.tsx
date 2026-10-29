import type { ReactNode } from "react"
import {
  AlertCircle,
  ArrowUpRight,
  CheckCircle2,
  Inbox,
  LoaderCircle
} from "lucide-react"
import { Button } from "@/components/ui/button"

export function PageHeader({
  title,
  description,
  eyebrow,
  actions
}: {
  title: string
  description?: ReactNode
  eyebrow?: string
  actions?: ReactNode
}) {
  return (
    <div className="workbench-page-header">
      <div className="workbench-heading">
        {eyebrow && <p className="workbench-eyebrow">{eyebrow}</p>}
        <h1 tabIndex={-1} data-page-title>
          {title}
        </h1>
        {description && <p className="workbench-description">{description}</p>}
      </div>
      {actions && <div className="workbench-header-actions">{actions}</div>}
    </div>
  )
}
export function ViewState({
  title,
  description,
  kind = "empty",
  action
}: {
  title: string
  description?: ReactNode
  kind?: "empty" | "error" | "loading" | "success"
  action?: ReactNode
}) {
  const Icon =
    kind === "error"
      ? AlertCircle
      : kind === "loading"
        ? LoaderCircle
        : kind === "success"
          ? CheckCircle2
          : Inbox
  return (
    <div
      className={`workbench-state workbench-state-${kind}`}
      role={
        kind === "error" ? "alert" : kind === "loading" ? "status" : undefined
      }
    >
      <span className="workbench-state-icon">
        <Icon
          className={kind === "loading" ? "animate-spin" : ""}
          aria-hidden="true"
        />
      </span>
      <h2>{title}</h2>
      {description && <p>{description}</p>}
      {action && <div className="workbench-state-action">{action}</div>}
    </div>
  )
}
export function ErrorState({
  title = "This view is temporarily unavailable",
  error,
  retry
}: {
  title?: string
  error?: unknown
  retry: () => void
}) {
  return (
    <ViewState
      kind="error"
      title={title}
      description={
        error instanceof Error
          ? error.message
          : "Your data is still here. Try loading this view again."
      }
      action={
        <Button variant="outline" onClick={retry}>
          Try again
        </Button>
      }
    />
  )
}
export function StatusBadge({
  children,
  tone = "neutral",
  dot = false
}: {
  children: ReactNode
  tone?: "neutral" | "positive" | "warning" | "danger" | "accent"
  dot?: boolean
}) {
  return (
    <span className="workbench-badge" data-tone={tone}>
      {dot && <span className="workbench-status-dot" aria-hidden="true" />}
      {children}
    </span>
  )
}
export function SectionHeading({
  title,
  description,
  action
}: {
  title: string
  description?: string
  action?: ReactNode
}) {
  return (
    <div className="workbench-section-heading">
      <div>
        <h2>{title}</h2>
        {description && <p>{description}</p>}
      </div>
      {action}
    </div>
  )
}
export function TextAction({
  children,
  onClick
}: {
  children: ReactNode
  onClick: () => void
}) {
  return (
    <button type="button" className="workbench-text-action" onClick={onClick}>
      {children}
      <ArrowUpRight aria-hidden="true" />
    </button>
  )
}
