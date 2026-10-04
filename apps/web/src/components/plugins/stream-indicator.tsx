import type { StreamState } from "@/lib/plugin-hooks"
import { cn } from "@/lib/utils"

const LABELS: Record<StreamState, { text: string; title: string; dot: string }> = {
  live: { text: "Live", title: "Run updates stream from the server", dot: "bg-emerald-500" },
  connecting: { text: "Connecting…", title: "Connecting to live run updates", dot: "bg-amber-400" },
  offline: { text: "Polling", title: "Live updates are unavailable; active runs are polled every few seconds", dot: "bg-zinc-400" },
}

export function StreamIndicator({ state }: { state: StreamState }) {
  const label = LABELS[state]
  return <span role="status" title={label.title} data-stream={state} className="inline-flex items-center gap-1.5 text-[11px] text-muted-foreground">
    <span aria-hidden="true" className={cn("size-1.5 rounded-full", label.dot)} />
    {label.text}<span className="sr-only">: {label.title}</span>
  </span>
}
