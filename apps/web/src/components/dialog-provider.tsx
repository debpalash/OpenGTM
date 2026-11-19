// In-app replacement for window.confirm / window.prompt. Mount <DialogProvider>
// once (App.tsx); call sites stay one line:
//   const confirm = useConfirm()
//   if (!(await confirm({ title: `Delete workbook “${name}”?`, confirmLabel: "Delete workbook", destructive: true }))) return
// Focus moves into the dialog and returns to the trigger on close (base-ui),
// Escape and the Cancel button resolve false / null.

import { createContext, useContext, useRef, useState, useSyncExternalStore, type ReactNode } from "react"
import { ConfirmDialog } from "@/components/confirm-dialog"
import { Input } from "@/components/ui/input"
import { DialogQueue, type ConfirmRequest, type PromptRequest } from "@/lib/dialog-queue"

const Context = createContext<DialogQueue | null>(null)

function useQueue() {
  const queue = useContext(Context)
  if (!queue) throw new Error("DialogProvider is required")
  return queue
}

export function useConfirm() {
  const queue = useQueue()
  return (request: Omit<ConfirmRequest, "kind">) => queue.askConfirm(request)
}

export function usePrompt() {
  const queue = useQueue()
  return (request: Omit<PromptRequest, "kind">) => queue.askPrompt(request)
}

function PromptBody({ request, onSubmit, onCancel }: {
  request: PromptRequest
  onSubmit: (value: string) => void
  onCancel: () => void
}) {
  const [value, setValue] = useState(request.defaultValue ?? "")
  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => { if (!open) onCancel() }}
      title={request.title}
      description={request.description}
      confirmLabel={request.confirmLabel}
      confirmDisabled={!value.trim()}
      onConfirm={() => onSubmit(value.trim())}
    >
      <Input
        autoFocus
        aria-label={request.label}
        placeholder={request.placeholder}
        value={value}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && value.trim()) {
            event.preventDefault()
            onSubmit(value.trim())
          }
        }}
      />
    </ConfirmDialog>
  )
}

export function DialogProvider({ children }: { children: ReactNode }) {
  const [queue] = useState(() => new DialogQueue())
  const request = useSyncExternalStore(queue.subscribe, queue.getSnapshot)
  const opener = useRef<HTMLElement | null>(null)

  // Remember the trigger as the dialog opens; base-ui restores focus on close,
  // this only covers the case where focus fell back to <body>.
  if (request && !opener.current && document.activeElement instanceof HTMLElement && document.activeElement !== document.body) {
    opener.current = document.activeElement
  }
  const settle = (value: boolean | string | null) => {
    const target = opener.current
    opener.current = null
    queue.settle(value)
    setTimeout(() => {
      if (target?.isConnected && (!document.activeElement || document.activeElement === document.body)) target.focus()
    }, 0)
  }

  return (
    <Context.Provider value={queue}>
      {children}
      {request?.kind === "confirm" && (
        <ConfirmDialog
          open
          onOpenChange={(open) => { if (!open) settle(false) }}
          title={request.title}
          description={request.description}
          confirmLabel={request.confirmLabel}
          cancelLabel={request.cancelLabel}
          destructive={request.destructive}
          onConfirm={() => settle(true)}
        />
      )}
      {request?.kind === "prompt" && (
        <PromptBody key={request.title} request={request} onSubmit={settle} onCancel={() => settle(null)} />
      )}
    </Context.Provider>
  )
}
