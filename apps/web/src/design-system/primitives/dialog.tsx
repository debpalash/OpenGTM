// Sectioned dialogs for workbook surfaces, composed from the shadcn/ui
// Dialog and AlertDialog (Base UI). Popup → Header / Body / Footer keeps the
// spacing these screens were laid out with: 24px sections, right-aligned
// actions, no corner close button, and a body that scrolls within the
// viewport.

import type { ComponentProps } from "react"

import {
  Dialog as UiDialog, DialogClose, DialogContent, DialogDescription, DialogTitle, DialogTrigger,
} from "@/components/ui/dialog"
import {
  AlertDialog as UiAlertDialog, AlertDialogClose, AlertDialogContent, AlertDialogDescription,
  AlertDialogTitle, AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { cn } from "@/lib/utils"

export type DialogSize = "sm" | "md" | "lg" | "xl"

const popupClass = "flex max-h-[calc(100dvh-2rem)] max-w-[calc(100%-2rem)] flex-col gap-0 overflow-auto rounded-[var(--t-border-radius-md)] p-0 leading-[var(--gtm-line-height-body)] sm:max-w-[calc(100%-2rem)]"
const sizeClass: Record<DialogSize, string> = {
  sm: "w-[300px]",
  md: "w-[400px]",
  lg: "w-[max(400px,53%)]",
  xl: "w-[1200px]",
}
// Dialog titles are centered above the content; alert titles align left.
const dialogTitleClass = "mb-4 text-center text-base leading-[1.1] font-semibold"
const alertTitleClass = "text-base leading-[1.25] font-semibold"
const descriptionClass = "leading-normal"

function Section({ className, ...props }: ComponentProps<"div">) {
  return <div className={cn("flex shrink-0 flex-col gap-2 p-6", className)} {...props} />
}
function Body({ className, ...props }: ComponentProps<"div">) {
  return <div className={cn("px-6 pb-6", className)} {...props} />
}
function Footer({ className, ...props }: ComponentProps<"div">) {
  return <div className={cn("mt-auto flex shrink-0 flex-wrap items-center justify-end gap-2 px-6 pb-6", className)} {...props} />
}

function DialogPopup({ size = "md", className, ...props }: ComponentProps<typeof DialogContent> & { size?: DialogSize }) {
  return <DialogContent showCloseButton={false} data-size={size} className={cn(popupClass, sizeClass[size], className)} {...props} />
}
function AlertDialogPopup({ size = "md", className, ...props }: ComponentProps<typeof AlertDialogContent> & { size?: DialogSize }) {
  return <AlertDialogContent data-size={size} className={cn(popupClass, sizeClass[size], className)} {...props} />
}

export const Dialog = {
  Root: UiDialog,
  Trigger: DialogTrigger,
  Popup: DialogPopup,
  Header: Section,
  Title: ({ className, ...props }: ComponentProps<typeof DialogTitle>) => <DialogTitle className={cn(dialogTitleClass, className)} {...props} />,
  Description: ({ className, ...props }: ComponentProps<typeof DialogDescription>) => <DialogDescription className={cn(descriptionClass, className)} {...props} />,
  Body,
  Footer,
  Close: DialogClose,
}

export const AlertDialog = {
  Root: UiAlertDialog,
  Trigger: AlertDialogTrigger,
  Popup: AlertDialogPopup,
  Header: Section,
  Title: ({ className, ...props }: ComponentProps<typeof AlertDialogTitle>) => <AlertDialogTitle className={cn(alertTitleClass, className)} {...props} />,
  Description: ({ className, ...props }: ComponentProps<typeof AlertDialogDescription>) => <AlertDialogDescription className={cn(descriptionClass, className)} {...props} />,
  Body,
  Footer,
  Close: AlertDialogClose,
}
