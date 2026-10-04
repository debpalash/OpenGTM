import { Ban, CheckCircle2, CircleDashed, Loader2, XCircle, type LucideIcon } from "lucide-react"
import { cn } from "@/lib/utils"

const STATUS: Record<string, { label: string; icon: LucideIcon; className: string; spin?: boolean }> = {
  pending: { label: "Queued", icon: CircleDashed, className: "bg-muted text-muted-foreground" },
  running: { label: "Running", icon: Loader2, spin: true, className: "bg-sky-500/10 text-sky-700 dark:text-sky-300" },
  completed: { label: "Completed", icon: CheckCircle2, className: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" },
  failed: { label: "Failed", icon: XCircle, className: "bg-destructive/10 text-destructive" },
  cancelled: { label: "Cancelled", icon: Ban, className: "bg-muted text-muted-foreground" },
}

export function RunStatusBadge({ status, className }: { status: string; className?: string }) {
  const config = STATUS[status] ?? { label: status, icon: CircleDashed, className: "bg-muted text-muted-foreground" }
  const Icon = config.icon
  return (
    <span data-status={status} className={cn("inline-flex h-5 items-center gap-1 rounded-sm px-1.5 text-xs font-medium whitespace-nowrap", config.className, className)}>
      <Icon aria-hidden="true" className={cn("size-3", config.spin && "motion-safe:animate-spin")} />
      {config.label}
    </span>
  )
}
