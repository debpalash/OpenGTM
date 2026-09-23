import * as React from "react"
import { ChevronsUpDown } from "lucide-react"

import { cn } from "@/lib/utils"

type NativeSelectProps = Omit<React.ComponentProps<"select">, "size"> & {
  /** Control height: sm 24px, default 28px (32px on touch widths). */
  size?: "sm" | "default"
  /** Classes for the select element itself; `className` sizes the wrapper. */
  selectClassName?: string
}

/**
 * Token-styled native <select>: keeps native keyboard, screen-reader and
 * mobile picker behavior, styled as a macOS pop-up button (bezel + up/down chevron).
 */
function NativeSelect({ className, selectClassName, size = "default", children, ...props }: NativeSelectProps) {
  return (
    <div data-slot="native-select" className={cn("relative inline-flex min-w-0", className)}>
      <select
        className={cn(
          "w-full min-w-0 appearance-none rounded-md border-0 bg-[var(--gtm-control-bezel)] pr-7 pl-2.5 text-foreground shadow-[var(--gtm-control-shadow)] transition-[background-color,box-shadow] outline-none hover:bg-[color-mix(in_srgb,var(--gtm-control-bezel)_94%,var(--foreground))] focus-visible:outline-[3px] focus-visible:outline-offset-0 focus-visible:outline-ring disabled:cursor-not-allowed disabled:opacity-40 aria-invalid:outline-destructive",
          size === "sm" ? "h-[22px] text-xs" : "h-8 text-sm sm:h-7",
          selectClassName
        )}
        {...props}
      >
        {children}
      </select>
      <ChevronsUpDown aria-hidden="true" className="pointer-events-none absolute top-1/2 right-2 size-3 -translate-y-1/2 text-muted-foreground" />
    </div>
  )
}

export { NativeSelect }
