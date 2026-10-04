import type { ComponentProps, ReactNode } from "react"
import { LoaderCircle } from "lucide-react"

import { Button as UiButton } from "@/components/ui/button"
import { cn } from "@/lib/utils"

type UiButtonProps = ComponentProps<typeof UiButton>

export type ButtonProps = Omit<UiButtonProps, "variant" | "size" | "color"> & {
  /** `outline` (default) is the raised bezel; `solid` is the filled default action. */
  variant?: "solid" | "outline" | "ghost" | "soft"
  color?: "neutral" | "accent" | "danger" | "success"
  size?: "sm" | "md"
  /** Disables the button, sets aria-busy and shows a spinner over the label. */
  loading?: boolean
  startIcon?: ReactNode
  endIcon?: ReactNode
  fullWidth?: boolean
}

function uiVariant(variant: NonNullable<ButtonProps["variant"]>, color: NonNullable<ButtonProps["color"]>): UiButtonProps["variant"] {
  if (variant === "solid") return color === "danger" ? "destructive" : "default"
  if (variant === "ghost") return "ghost"
  if (variant === "soft") return "secondary"
  return "outline"
}

/** Workbook-surface button: the shadcn/ui Button with loading and icon slots. */
export function Button({
  variant = "outline", color = "neutral", size = "md", loading = false, startIcon, endIcon, fullWidth = false,
  disabled, className, children, ...props
}: ButtonProps) {
  return <UiButton
    {...props}
    variant={uiVariant(variant, color)}
    size={size === "sm" ? "sm" : "default"}
    disabled={disabled || loading}
    aria-busy={loading || props["aria-busy"] || undefined}
    data-loading={loading || undefined}
    // 32px regular / 24px small, 8px insets, medium corners, and the fill
    // running under the hairline: the metrics these screens were laid out with.
    className={cn("relative rounded-[var(--t-border-radius-md)] bg-clip-border px-2", size === "sm" ? "h-6" : "h-8", variant !== "solid" && color === "danger" && "text-destructive", fullWidth && "w-full", className)}
  >
    <span className={cn("inline-flex min-w-0 items-center gap-1.5", loading && "invisible")}>
      {startIcon && <span className="inline-flex" aria-hidden="true">{startIcon}</span>}
      {children}
      {endIcon && <span className="inline-flex" aria-hidden="true">{endIcon}</span>}
    </span>
    {loading && <span className="absolute inset-0 flex items-center justify-center" aria-hidden="true">
      <LoaderCircle className="animate-spin" />
    </span>}
  </UiButton>
}
