// Automations (rules) page — SCAFFOLD PLACEHOLDER.
//
// The real RuleBuilder / TriggerList / RunDetail land in the parallel slice
// phase. This placeholder wires up the shared gating contract so the route
// compiles, navigates, and renders the correct state today:
//   * flag off → 404 → <FeatureDisabled variant="flag-off"> (never blank)
//   * otherwise → <ComingSoon>
//
// It probes the live state via useTriggers() (404 when AUTOMATIONS_ENABLED off)
// and useFlags() so the screen matches reality. Slice agents replace the
// ComingSoon body with the real list/builder/detail views.

import { ComingSoon } from "@/components/coming-soon"
import {
  FeatureDisabled,
  FeatureDisabledFromError,
  isFeatureDisabledError,
} from "@/components/feature-disabled"
import { ErrorState, Loading } from "@/components/states"
import { useFlags, useTriggers } from "@/lib/automation-hooks"

const FEATURE = "Automations"

export default function AutomationsPage() {
  const flags = useFlags()
  const triggers = useTriggers()

  // Flag-off (404) → feature-disabled screen, driven by the page query error.
  if (isFeatureDisabledError(triggers.error)) {
    return <FeatureDisabledFromError error={triggers.error} feature={FEATURE} />
  }
  // Belt-and-suspenders: proactive flag check (avoids a flash before the query).
  if (flags.data && !flags.data.automations_enabled) {
    return <FeatureDisabled variant="flag-off" feature={FEATURE} />
  }

  if (triggers.isLoading) {
    return (
      <div className="p-4">
        <Loading rows={4} />
      </div>
    )
  }
  if (triggers.isError) {
    return (
      <div className="p-4">
        <ErrorState error={triggers.error} onRetry={() => triggers.refetch()} />
      </div>
    )
  }

  return (
    <div className="p-4">
      <ComingSoon feature={FEATURE} />
    </div>
  )
}
