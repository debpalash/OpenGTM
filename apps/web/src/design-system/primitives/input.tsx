import type { ComponentProps } from "react"
import { Input as AppInput } from "@/components/ui/input"
import { cn } from "@/lib/utils"

/** App-owned input with the existing design-system call signature. */
export function Input({
  className,
  size = "md",
  ...props
}: Omit<ComponentProps<typeof AppInput>, "size"> & { size?: "sm" | "md" }) {
  return (
    <AppInput {...props} className={cn(size === "sm" && "h-8", className)} />
  )
}
