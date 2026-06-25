// Single source-of-truth status → color map for every status string the GTM
// surfaces emit (sequence / run / send / watch). Promoted from outreach.tsx and
// extended per spec §4. The `satisfies` assertion guarantees, at compile time,
// that every known status key is mapped — a missing key fails `tsc`.

// ── Known status unions (mirror the verified backend status strings) ────────
export type SequenceStatus = "draft" | "active" | "paused" | "completed"
export type RunStatus = "queued" | "running" | "succeeded" | "failed" | "skipped"
export type SendStatus =
  | "pending"
  | "scheduled"
  | "sent"
  | "opened"
  | "replied"
  | "bounced"
  | "failed"
  | "skipped"
  | "suppressed"
  | "completed"
export type WatchStatus = "enabled" | "disabled"

export type StatusKey = SequenceStatus | RunStatus | SendStatus | WatchStatus

const NEUTRAL = "bg-zinc-400"

// Every known status is mapped (compile-time enforced via `satisfies`).
export const STATUS_COLORS = {
  // sequence
  draft: "bg-zinc-400",
  active: "bg-emerald-400",
  paused: "bg-amber-400",
  completed: "bg-blue-400",
  // run
  queued: "bg-zinc-400",
  running: "bg-sky-400",
  succeeded: "bg-emerald-400",
  failed: "bg-rose-400",
  skipped: "bg-zinc-500",
  // send
  pending: "bg-zinc-400",
  scheduled: "bg-sky-400",
  sent: "bg-blue-400",
  opened: "bg-violet-400",
  replied: "bg-emerald-400",
  bounced: "bg-rose-400",
  suppressed: "bg-rose-500",
  // watch
  enabled: "bg-emerald-400",
  disabled: "bg-zinc-400",
} satisfies Record<StatusKey, string>

export function statusColor(status: string): string {
  return (STATUS_COLORS as Record<string, string>)[status] ?? NEUTRAL
}

export function StatusDot({ status }: { status: string }) {
  return (
    <span className="inline-flex items-center gap-1 text-[10px] text-muted-foreground">
      <span className={`size-1.5 rounded-full ${statusColor(status)}`} />
      {status}
    </span>
  )
}
