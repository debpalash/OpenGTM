// Centered "feature off" / "dependency missing" screen for flagged pages.
//
// Used by Automations + Watches when the page-level query throws an ApiError:
//   * 404 (flag off)  → variant="flag-off"  ("Automations is turned off …")
//   * 409 (PG dep)    → variant="dependency" ("Intent Watches require Postgres …")
// `fromError()` picks the right variant + copy from a thrown ApiError so pages
// never render blank.

import { Lock, PlugZap } from "lucide-react"
import { ApiError } from "@/lib/api"

export type FeatureDisabledVariant = "flag-off" | "dependency"

interface FeatureDisabledProps {
  variant: FeatureDisabledVariant
  feature: string
  /** Override the body copy (falls back to a sensible default per variant). */
  detail?: string
  docHref?: string
}

export function FeatureDisabled({
  variant,
  feature,
  detail,
  docHref,
}: FeatureDisabledProps) {
  const Icon = variant === "dependency" ? PlugZap : Lock
  const title =
    variant === "dependency"
      ? `${feature} needs an additional dependency`
      : `${feature} is turned off`
  const body =
    detail ??
    (variant === "dependency"
      ? `${feature} requires the Postgres lead store (PG_LEAD_STORE). Ask an administrator to enable it.`
      : `${feature} is disabled for this deployment. Ask an administrator to enable the feature flag.`)

  return (
    <div className="flex h-full min-h-[40vh] flex-col items-center justify-center gap-3 p-8 text-center">
      <div className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <Icon className="size-5" />
      </div>
      <h2 className="text-base font-semibold">{title}</h2>
      <p className="max-w-md text-xs text-muted-foreground">{body}</p>
      {docHref && (
        <a
          href={docHref}
          className="text-xs text-primary underline-offset-4 hover:underline"
          target="_blank"
          rel="noreferrer"
        >
          Learn more
        </a>
      )}
    </div>
  )
}

/** True when this error should render a FeatureDisabled screen (404/409). */
export function isFeatureDisabledError(err: unknown): err is ApiError {
  return err instanceof ApiError && (err.status === 404 || err.status === 409)
}

/** Render a FeatureDisabled from a thrown ApiError, or null if not applicable. */
export function FeatureDisabledFromError({
  error,
  feature,
}: {
  error: unknown
  feature: string
}) {
  if (!isFeatureDisabledError(error)) return null
  const variant: FeatureDisabledVariant =
    error.status === 409 ? "dependency" : "flag-off"
  // For 409 the backend detail is the canonical reason
  // (e.g. intent_poller_requires_pg_lead_store); show our friendly copy.
  return <FeatureDisabled variant={variant} feature={feature} />
}
