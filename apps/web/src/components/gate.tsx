// Role-gating wrapper for write controls (spec §6).
//
// Now that GET /api/me/context exposes the caller's per-workspace role, we can
// gate PROACTIVELY: <Gate need="admin"> disables its child control and explains
// why via a tooltip when the caller lacks the role. The reactive 403 handler in
// the mutation hooks remains the authoritative backstop.
//
//   <Gate need="admin">
//     <Button onClick={start}>Send now</Button>
//   </Gate>
//
// While the role is still loading we leave controls enabled (optimistic) — the
// 403 backstop covers the race.

import { cloneElement, isValidElement, type ReactElement } from "react"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { useRole } from "@/lib/automation-hooks"

export type RoleNeed = "admin" | "editor"

const NEED_MESSAGE: Record<RoleNeed, string> = {
  admin: "This action requires the admin role for this workspace.",
  editor: "This action requires the editor or admin role for this workspace.",
}

/** True when the role satisfies the requirement. */
export function satisfiesRole(
  need: RoleNeed,
  caps: { canAdmin: boolean; canEdit: boolean },
): boolean {
  return need === "admin" ? caps.canAdmin : caps.canEdit
}

interface GateProps {
  need: RoleNeed
  /** A single control (Button-like) that accepts `disabled`. */
  children: ReactElement<{ disabled?: boolean }>
  /** Override the disabled tooltip copy. */
  reason?: string
}

export function Gate({ need, children, reason }: GateProps) {
  const { canAdmin, canEdit, isLoading } = useRole()
  const allowed = isLoading || satisfiesRole(need, { canAdmin, canEdit })

  if (allowed || !isValidElement(children)) return children

  const disabledChild = cloneElement(children, { disabled: true })
  return (
    <Tooltip>
      {/* wrapper span keeps the tooltip reachable on a disabled control */}
      <TooltipTrigger
        render={
          <span className="inline-flex cursor-not-allowed">{disabledChild}</span>
        }
      />
      <TooltipContent>{reason ?? NEED_MESSAGE[need]}</TooltipContent>
    </Tooltip>
  )
}

/** Hook form: returns whether the caller may perform a role-gated action. */
export function useCanRole(need: RoleNeed): { allowed: boolean; reason: string } {
  const { canAdmin, canEdit, isLoading } = useRole()
  const allowed = isLoading || satisfiesRole(need, { canAdmin, canEdit })
  return { allowed, reason: NEED_MESSAGE[need] }
}
