import type { ComponentProps } from "react"
import type { Button as TwentyButton } from "twenty-ui/primitives/input"
import { Loader2 } from "lucide-react"
import { Button as AppButton } from "@/components/ui/button"
import { cn } from "@/lib/utils"

// Compatibility boundary for existing compound dialogs. Both entry points
// render the same app-owned control while callers migrate at their own pace.
export function Button({
  variant = "outline",
  color = "neutral",
  size = "md",
  loading = false,
  fullWidth,
  elevated,
  startIcon,
  endIcon,
  hotkeys,
  soon,
  soonLabel,
  href,
  target,
  rel,
  download,
  render,
  disabled,
  className,
  children,
  ...props
}: ComponentProps<typeof TwentyButton>) {
  void elevated
  const mapped =
    variant === "solid"
      ? color === "danger"
        ? "destructive"
        : "default"
      : variant === "soft"
        ? "secondary"
        : variant
  return (
    <AppButton
      {...props}
      render={
        href ? (
          <a href={href} target={target} rel={rel} download={download} />
        ) : (
          render
        )
      }
      nativeButton={!href}
      variant={mapped}
      size={size === "sm" ? "sm" : "default"}
      disabled={disabled || loading || soon}
      aria-busy={loading || undefined}
      className={cn(
        fullWidth && "w-full",
        color === "danger" && variant !== "solid" && "text-[var(--gtm-danger)]",
        color === "accent" && variant !== "solid" && "text-[var(--gtm-link)]",
        color === "success" && "text-[var(--gtm-success)]",
        className
      )}
    >
      {loading ? (
        <Loader2 aria-hidden="true" className="size-4 animate-spin" />
      ) : (
        startIcon
      )}
      {children}
      {endIcon}
      {hotkeys?.map((key) => (
        <kbd key={key} className="gtm-kbd">
          {key}
        </kbd>
      ))}
      {soon && <span>{soonLabel || "Coming soon"}</span>}
    </AppButton>
  )
}
