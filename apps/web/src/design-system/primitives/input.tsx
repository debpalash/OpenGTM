import type { ComponentProps } from "react"

import { Input as UiInput } from "@/components/ui/input"
import { cn } from "@/lib/utils"

/**
 * Workbook-surface text field: the shadcn/ui Input with the macOS field
 * treatment these screens used — accent border and system focus ring on
 * focus, tertiary placeholder, 13px text.
 */
export function Input({ className, ...props }: ComponentProps<typeof UiInput>) {
  return <UiInput
    className={cn(
      "rounded-[var(--t-border-radius-md)] px-2 text-sm placeholder:text-[var(--t-font-color-tertiary)] focus:border-[var(--t-color-blue)] focus-visible:border-[var(--t-color-blue)] focus-visible:outline-solid focus-visible:outline-[var(--gtm-focus-ring)]",
      className,
    )}
    {...props}
  />
}
