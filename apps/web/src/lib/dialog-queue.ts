// Promise-based queue behind useConfirm()/usePrompt(). Pure state (no DOM) so it
// can be unit tested; the provider in components/dialog-provider.tsx renders it.

export interface ConfirmRequest {
  kind: "confirm"
  title: string
  description?: string
  /** Name the action ("Delete workbook"), never "OK". */
  confirmLabel: string
  cancelLabel?: string
  destructive?: boolean
}

export interface PromptRequest {
  kind: "prompt"
  title: string
  description?: string
  /** Accessible label for the text field. */
  label: string
  confirmLabel: string
  defaultValue?: string
  placeholder?: string
}

export type DialogRequest = ConfirmRequest | PromptRequest

interface Entry {
  request: DialogRequest
  resolve: (value: boolean | string | null) => void
}

export class DialogQueue {
  private entries: Entry[] = []
  private listeners = new Set<() => void>()
  private snapshot: DialogRequest | null = null

  subscribe = (listener: () => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }

  /** The request currently on screen (stable reference until it changes). */
  getSnapshot = () => this.snapshot

  askConfirm(request: Omit<ConfirmRequest, "kind">): Promise<boolean> {
    return this.enqueue({ kind: "confirm", ...request }) as Promise<boolean>
  }

  askPrompt(request: Omit<PromptRequest, "kind">): Promise<string | null> {
    return this.enqueue({ kind: "prompt", ...request }) as Promise<string | null>
  }

  /** Resolve the visible request. Cancel is `false` for confirm and `null` for prompt. */
  settle(value: boolean | string | null) {
    const entry = this.entries.shift()
    if (!entry) return
    entry.resolve(entry.request.kind === "confirm" ? value === true : typeof value === "string" ? value : null)
    this.publish()
  }

  private enqueue(request: DialogRequest) {
    return new Promise<boolean | string | null>((resolve) => {
      this.entries.push({ request, resolve })
      this.publish()
    })
  }

  private publish() {
    const next = this.entries[0]?.request ?? null
    if (next === this.snapshot) return
    this.snapshot = next
    for (const listener of this.listeners) listener()
  }
}
