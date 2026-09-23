/**
 * One icon per product concept, so tiers, contact channels, statuses and
 * yes/no marks look the same on every screen. Lucide only — no emoji.
 */
import type { ComponentProps, ComponentType } from "react"
import {
  Check, CircleCheck, CircleDashed, CircleOff, CircleX, Clock, Flame, Globe,
  LoaderCircle, Mail, Phone, Snowflake, Thermometer, User, X,
  type LucideProps,
} from "lucide-react"

import { cn } from "@/lib/utils"

type IconComponent = ComponentType<LucideProps>

/** LinkedIn mark (Lucide 1.x dropped brand icons); sized and colored like Lucide. */
export function LinkedInIcon({ className, size = 24, ...props }: ComponentProps<"svg"> & { size?: number | string }) {
  return (
    <svg viewBox="0 0 24 24" width={size} height={size} fill="currentColor" aria-hidden="true"
      className={cn("lucide", className)} {...props}>
      <path d="M4.98 3.5a2.5 2.5 0 1 1 0 5 2.5 2.5 0 0 1 0-5ZM3 9.75h4v11H3v-11Zm6.5 0h3.83v1.5h.05c.53-.95 1.84-1.95 3.79-1.95 4.05 0 4.83 2.5 4.83 5.76v5.69h-4v-5.04c0-1.2-.02-2.75-1.73-2.75-1.73 0-2 1.3-2 2.66v5.13h-4v-11Z" />
    </svg>
  )
}

// ── Lead score tier ────────────────────────────────────────────────────────

export type Tier = "hot" | "warm" | "cold" | "unqualified"

export const TIER_META: Record<Tier, { label: string; icon: IconComponent; className: string }> = {
  hot: { label: "Hot", icon: Flame, className: "text-[var(--t-color-red9)]" },
  warm: { label: "Warm", icon: Thermometer, className: "text-[var(--t-color-orange9)]" },
  cold: { label: "Cold", icon: Snowflake, className: "text-[var(--t-color-blue9)]" },
  unqualified: { label: "Unqualified", icon: CircleOff, className: "text-muted-foreground" },
}

export function TierIcon({ tier, className }: { tier: string | null | undefined; className?: string }) {
  const meta = TIER_META[(tier as Tier) in TIER_META ? (tier as Tier) : "unqualified"]
  const Icon = meta.icon
  return <Icon aria-hidden="true" className={cn("size-3.5 shrink-0", meta.className, className)} />
}

// ── Contact channels ───────────────────────────────────────────────────────

export type Channel = "email" | "phone" | "linkedin" | "website" | "contact"

export const CHANNEL_META: Record<Channel, { label: string; icon: IconComponent | typeof LinkedInIcon }> = {
  email: { label: "Email", icon: Mail },
  phone: { label: "Phone", icon: Phone },
  linkedin: { label: "LinkedIn", icon: LinkedInIcon },
  website: { label: "Website", icon: Globe },
  contact: { label: "Contact", icon: User },
}

export function ChannelIcon({ channel, className }: { channel: Channel; className?: string }) {
  const Icon = CHANNEL_META[channel].icon
  return <Icon aria-hidden="true" className={cn("size-3.5 shrink-0", className)} />
}

// ── Job / task status ──────────────────────────────────────────────────────

export type RunStatus = "pending" | "running" | "done" | "failed" | "skipped"

const STATUS_META: Record<RunStatus, { label: string; icon: IconComponent; className: string }> = {
  pending: { label: "Pending", icon: Clock, className: "text-muted-foreground" },
  running: { label: "Running", icon: LoaderCircle, className: "animate-spin text-[var(--t-color-blue9)]" },
  done: { label: "Done", icon: CircleCheck, className: "text-[var(--t-color-green9)]" },
  failed: { label: "Failed", icon: CircleX, className: "text-destructive" },
  skipped: { label: "Skipped", icon: CircleDashed, className: "text-muted-foreground" },
}

export function StatusIcon({ status, className }: { status: RunStatus; className?: string }) {
  const meta = STATUS_META[status] ?? STATUS_META.pending
  const Icon = meta.icon
  return <Icon aria-label={meta.label} role="img" className={cn("size-3.5 shrink-0", meta.className, className)} />
}

// ── Yes / no marks ─────────────────────────────────────────────────────────

export function BoolMark({ value, label, className }: { value: boolean; label?: string; className?: string }) {
  const Icon = value ? Check : X
  return (
    <Icon role="img" aria-label={label ?? (value ? "Yes" : "No")}
      className={cn("size-3.5 shrink-0", value ? "text-[var(--t-color-green9)]" : "text-muted-foreground", className)} />
  )
}
