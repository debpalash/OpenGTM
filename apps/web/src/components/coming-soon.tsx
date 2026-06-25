// Placeholder page body for not-yet-built feature slices (Automations, Watches).
// The real pages replace this in the parallel slice phase. It still respects the
// shared gating contract: callers wrap it so flag-off / dependency states render
// FeatureDisabled instead.

import { Construction } from "lucide-react"

export function ComingSoon({ feature }: { feature: string }) {
  return (
    <div className="flex h-full min-h-[40vh] flex-col items-center justify-center gap-3 p-8 text-center">
      <div className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <Construction className="size-5" />
      </div>
      <h2 className="text-base font-semibold">{feature}</h2>
      <p className="max-w-md text-xs text-muted-foreground">
        This page is coming soon. The {feature} experience is being built on top of
        the shared scaffold.
      </p>
    </div>
  )
}
