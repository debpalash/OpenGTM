import * as React from "react"

import { cn } from "@/lib/utils"

function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        "flex field-sizing-content min-h-16 w-full rounded-md border border-border bg-background px-2.5 py-2 text-base transition-colors outline-none placeholder:text-muted-foreground focus-visible:border-transparent focus-visible:outline-[3px] focus-visible:outline-offset-0 focus-visible:outline-ring disabled:cursor-not-allowed disabled:bg-muted disabled:opacity-50 aria-invalid:border-destructive  md:text-sm",
        className
      )}
      {...props}
    />
  )
}

export { Textarea }
