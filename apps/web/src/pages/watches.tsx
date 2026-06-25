// Intent Watches page — SCAFFOLD PLACEHOLDER.
//
// The real WatchList / WatchBuilder / WatchDetail land in the parallel slice
// phase. This placeholder wires the shared gating contract so the route compiles,
// navigates, and renders the correct state today:
//   * poller off    → 404 → <FeatureDisabled variant="flag-off">
//   * PG store off  → 409 → <FeatureDisabled variant="dependency"> (distinct copy)
//   * otherwise     → <ComingSoon>
//
// State is probed via useWatches() (which 404s when INTENT_POLLER_ENABLED off and
// 409s when PG_LEAD_STORE off) and useFlags(). Slice agents replace the
// ComingSoon body with the real list/builder/detail views.

import { ComingSoon } from "@/components/coming-soon"
import {
  FeatureDisabled,
  FeatureDisabledFromError,
  isFeatureDisabledError,
} from "@/components/feature-disabled"
import { ErrorState, Loading } from "@/components/states"
import { useFlags, useWatches } from "@/lib/automation-hooks"

const FEATURE = "Intent Watches"

export default function WatchesPage() {
  const flags = useFlags()
  const watches = useWatches()

  // 404 (flag off) → flag-off screen; 409 (PG dep) → dependency screen.
  if (isFeatureDisabledError(watches.error)) {
    return <FeatureDisabledFromError error={watches.error} feature={FEATURE} />
  }
  // Proactive flag checks (avoid a flash before the query resolves).
  if (flags.data && !flags.data.intent_poller_enabled) {
    return <FeatureDisabled variant="flag-off" feature={FEATURE} />
  }
  if (flags.data && !flags.data.pg_lead_store) {
    return <FeatureDisabled variant="dependency" feature={FEATURE} />
  }

  if (watches.isLoading) {
    return (
      <div className="p-4">
        <Loading rows={4} />
      </div>
    )
  }
  if (watches.isError) {
    return (
      <div className="p-4">
        <ErrorState error={watches.error} onRetry={() => watches.refetch()} />
      </div>
    )
  }

  return (
    <div className="p-4">
      <ComingSoon feature={FEATURE} />
    </div>
  )
}
