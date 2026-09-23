import { Tabs as TabsPrimitive } from "@base-ui/react/tabs"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

function Tabs({
  className,
  orientation = "horizontal",
  ...props
}: TabsPrimitive.Root.Props) {
  return (
    <TabsPrimitive.Root
      data-slot="tabs"
      data-orientation={orientation}
      className={cn(
        "group/tabs flex gap-2 data-horizontal:flex-col",
        className
      )}
      {...props}
    />
  )
}

// Default: Twenty/Notion underline tabs on a hairline. "segmented" keeps the
// compact pill switcher for in-card toggles.
// macOS segmented control by default; "line" keeps underline tabs for dense panels.
const tabsListVariants = cva(
  "group/tabs-list inline-flex w-fit items-center text-muted-foreground group-data-vertical/tabs:h-fit group-data-vertical/tabs:flex-col",
  {
    variants: {
      variant: {
        default: "h-7 justify-center gap-0.5 rounded-md bg-[var(--t-background-transparent-medium)] p-0.5",
        segmented: "h-7 justify-center gap-0.5 rounded-md bg-[var(--t-background-transparent-medium)] p-0.5",
        line: "h-9 gap-1 border-b border-border group-data-horizontal/tabs:w-full justify-start",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function TabsList({
  className,
  variant = "default",
  ...props
}: TabsPrimitive.List.Props & VariantProps<typeof tabsListVariants>) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      data-variant={variant}
      className={cn(tabsListVariants({ variant }), className)}
      {...props}
    />
  )
}

function TabsTrigger({ className, ...props }: TabsPrimitive.Tab.Props) {
  return (
    <TabsPrimitive.Tab
      data-slot="tabs-trigger"
      className={cn(
        "relative inline-flex items-center justify-center gap-1.5 rounded-[var(--t-border-radius-sm)] px-3 text-sm font-medium whitespace-nowrap text-muted-foreground transition-[background-color,color,box-shadow] duration-150 outline-none hover:text-foreground focus-visible:outline-[3px] focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-40 aria-disabled:pointer-events-none aria-disabled:opacity-40 group-data-vertical/tabs:w-full group-data-vertical/tabs:justify-start [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-3.5 data-active:text-foreground",
        // Segmented: the selected segment is a raised bezel.
        "group-data-[variant=default]/tabs-list:h-full group-data-[variant=segmented]/tabs-list:h-full group-data-[variant=default]/tabs-list:data-active:bg-[var(--gtm-control-bezel)] group-data-[variant=segmented]/tabs-list:data-active:bg-[var(--gtm-control-bezel)] group-data-[variant=default]/tabs-list:data-active:shadow-[var(--gtm-control-shadow)] group-data-[variant=segmented]/tabs-list:data-active:shadow-[var(--gtm-control-shadow)]",
        // Line: hover tint and a 2px indicator on the hairline.
        "group-data-[variant=line]/tabs-list:h-7 group-data-[variant=line]/tabs-list:px-2 group-data-[variant=line]/tabs-list:hover:bg-accent after:absolute after:inset-x-1 after:-bottom-[5px] after:h-0.5 after:rounded-full after:bg-[var(--gtm-accent)] after:opacity-0 group-data-[variant=line]/tabs-list:data-active:after:opacity-100",
        className
      )}
      {...props}
    />
  )
}

function TabsContent({ className, ...props }: TabsPrimitive.Panel.Props) {
  return (
    <TabsPrimitive.Panel
      data-slot="tabs-content"
      className={cn("flex-1 text-sm outline-none", className)}
      {...props}
    />
  )
}

export { Tabs, TabsList, TabsTrigger, TabsContent, tabsListVariants }
