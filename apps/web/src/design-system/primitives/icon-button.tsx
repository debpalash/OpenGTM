import type { ComponentProps, ReactNode } from "react"
import { Button } from "@/components/ui/button"

export type IconButtonProps = Omit<
  ComponentProps<typeof Button>,
  "aria-label" | "children" | "size"
> & {
  label: string
  children: ReactNode
  size?: "icon" | "icon-sm" | "icon-lg"
}

/** The accessible name is required at the application import seam. */
export function IconButton({
  label,
  size = "icon",
  children,
  ...props
}: IconButtonProps) {
  return (
    <Button {...props} size={size} aria-label={label}>
      {children}
    </Button>
  )
}
