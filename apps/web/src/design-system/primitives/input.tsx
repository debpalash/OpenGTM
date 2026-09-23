import type { ComponentProps } from "react"
import { Input as TwentyInput } from "twenty-ui/primitives/input"

import { cn } from "@/lib/utils"

/** Twenty's Input as a macOS text field (see .gtm-text-field in styles.css). */
export function Input({ className, ...props }: ComponentProps<typeof TwentyInput>) {
  return <TwentyInput {...props} className={cn("gtm-text-field", className)} />
}
