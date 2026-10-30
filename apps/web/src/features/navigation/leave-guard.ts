export interface LeaveBlock {
  dirty: boolean
  busy: boolean
  message?: string
  busyLabel?: string
}
export interface LeavePrompt {
  action: string
  dirtyCount: number
  busy: boolean
  messages: string[]
  busyLabel: string | null
}

/** Owns the decision before context changes can discard an editor or a save.
 * A second request cannot replace the action the user is already reviewing. */
export class LeaveGuard {
  private blocks = new Map<string, LeaveBlock>()
  private listeners = new Set<() => void>()
  private pending: {
    action: string
    resolve: (leave: boolean) => void
  } | null = null
  private snapshot: LeavePrompt | null = null

  subscribe = (listener: () => void) => {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }
  getSnapshot = () => this.snapshot
  isProtected = () =>
    [...this.blocks.values()].some((block) => block.dirty || block.busy)
  cancel = () => this.answer(false)

  set(id: string, block: LeaveBlock | null) {
    if (block) this.blocks.set(id, block)
    else this.blocks.delete(id)
    this.publish()
  }
  request = (action: string): Promise<boolean> => {
    if (this.pending) return Promise.resolve(false)
    if (!this.isProtected()) return Promise.resolve(true)
    return new Promise((resolve) => {
      this.pending = { action, resolve }
      this.publish()
    })
  }
  answer = (leave: boolean) => {
    if (!this.pending || (leave && this.snapshot?.busy)) return
    const { resolve } = this.pending
    this.pending = null
    this.publish()
    resolve(leave)
  }
  private publish() {
    const blocks = [...this.blocks.values()]
    const next = this.pending
      ? {
          action: this.pending.action,
          dirtyCount: blocks.filter((block) => block.dirty).length,
          busy: blocks.some((block) => block.busy),
          busyLabel:
            blocks.find((block) => block.busy && block.busyLabel)?.busyLabel ??
            null,
          messages: [
            ...new Set(
              blocks
                .filter((block) => block.dirty && block.message)
                .map((block) => block.message!)
            )
          ]
        }
      : null
    if (JSON.stringify(next) === JSON.stringify(this.snapshot)) return
    this.snapshot = next
    this.listeners.forEach((listener) => listener())
  }
}
