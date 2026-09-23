import { Button as ButtonPrimitive } from "@base-ui/react/button"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

// Twenty/Notion button scale: 6px radius, 13px medium text, 24/28/32/36px
// heights, soft token hovers, and one focus treatment shared with inputs.
// macOS push buttons: accent-filled default, raised bezel for secondary and
// outline, 28px regular / 24px small / 32px large, 7px corners, 13px labels.
const buttonVariants = cva(
  "group/button inline-flex shrink-0 items-center justify-center rounded-md border border-transparent bg-clip-padding text-sm font-medium whitespace-nowrap transition-[background-color,box-shadow,color,opacity] duration-150 outline-none select-none focus-visible:outline-[3px] focus-visible:outline-offset-0 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-40 aria-invalid:border-destructive [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default:
          "bg-primary text-primary-foreground shadow-[0_0.5px_1px_rgb(0_0_0/0.18),inset_0_0.5px_0_rgb(255_255_255/0.18)] hover:bg-[var(--gtm-accent-hover)] active:brightness-95",
        outline:
          "bg-[var(--gtm-control-bezel)] text-foreground shadow-[var(--gtm-control-shadow)] hover:bg-[color-mix(in_srgb,var(--gtm-control-bezel)_92%,var(--foreground))] active:brightness-95 aria-expanded:bg-accent",
        secondary:
          "bg-[var(--gtm-control-bezel)] text-secondary-foreground shadow-[var(--gtm-control-shadow)] hover:bg-[color-mix(in_srgb,var(--gtm-control-bezel)_92%,var(--foreground))] active:brightness-95",
        ghost:
          "text-muted-foreground hover:bg-accent hover:text-foreground aria-expanded:bg-accent aria-expanded:text-foreground",
        destructive:
          "bg-destructive text-white shadow-[0_0.5px_1px_rgb(0_0_0/0.18)] hover:brightness-110 focus-visible:outline-destructive/50",
        link: "h-auto px-0 text-[var(--gtm-accent)] hover:underline underline-offset-4",
      },
      size: {
        default:
          "h-7 gap-1.5 px-3 has-data-[icon=inline-end]:pr-2.5 has-data-[icon=inline-start]:pl-2.5",
        xs: "h-[22px] gap-1 rounded-sm px-2 text-xs has-data-[icon=inline-end]:pr-1.5 has-data-[icon=inline-start]:pl-1.5 [&_svg:not([class*='size-'])]:size-3",
        sm: "h-6 gap-1 px-2.5 has-data-[icon=inline-end]:pr-2 has-data-[icon=inline-start]:pl-2 [&_svg:not([class*='size-'])]:size-3.5",
        lg: "h-8 gap-2 px-4",
        icon: "size-7",
        "icon-xs": "size-[22px] rounded-sm [&_svg:not([class*='size-'])]:size-3",
        "icon-sm": "size-6 [&_svg:not([class*='size-'])]:size-3.5",
        "icon-lg": "size-8",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

function Button({
  className,
  variant = "default",
  size = "default",
  ...props
}: ButtonPrimitive.Props & VariantProps<typeof buttonVariants>) {
  return (
    <ButtonPrimitive
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button, buttonVariants }
