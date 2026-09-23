import * as React from "react"
import { ChevronDown } from "lucide-react"

import { cn } from "@/lib/utils"

type NativeSelectProps = Omit<React.ComponentProps<"select">, "size"> & {
  /** Control height: sm 24px, default 28px (32px on touch widths). */
  size?: "sm" | "default"
  /** Classes for the select element itself; `className` sizes the wrapper. */
  selectClassName?: string
}

/**
 * Token-styled native <select>: keeps native keyboard, screen-reader and
 * mobile picker behavior, with a Lucide chevron instead of the browser arrow.
 */
function NativeSelect({ className, selectClassName, size = "default", children, ...props }: NativeSelectProps) {
  return (
    <div data-slot="native-select" className={cn("relative inline-flex min-w-0", className)}>
      <select
        className={cn(
          "w-full min-w-0 appearance-none rounded-md border border-border bg-background pr-7 pl-2 text-foreground transition-colors outline-none hover:bg-accent focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-ring disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive",
          size === "sm" ? "h-6 text-xs" : "h-8 text-sm sm:h-7",
          selectClassName
        )}
        {...props}
      >
        {children}
      </select>
      <ChevronDown aria-hidden="true" className="pointer-events-none absolute top-1/2 right-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
    </div>
  )
}

export { NativeSelect }
