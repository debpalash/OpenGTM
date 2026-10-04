import { Server } from "lucide-react"

/** Shown when /api/v2 is not served here (the legacy nginx/FastAPI stack). */
export function PlatformUnavailable() {
  return (
    <div className="flex min-h-[40vh] flex-col items-center justify-center gap-3 p-8 text-center" data-testid="plugin-platform-unavailable">
      <div className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <Server aria-hidden="true" className="size-5" />
      </div>
      <h2 className="text-base font-semibold">The plugin platform needs the Go server</h2>
      <p className="max-w-md text-xs text-muted-foreground">
        This dashboard is served by the legacy API, which does not provide <code className="font-mono">/api/v2</code>.
        Start the OpenGTM Go server and open the dashboard through it to browse plugins and run them.
      </p>
      <pre className="rounded-md bg-muted px-3 py-2 text-left font-mono text-xs select-all">docker compose --profile server up -d server</pre>
      <p className="max-w-md text-xs text-muted-foreground">
        Then open the dashboard from the Go server (port <code className="font-mono">3080</code> by default, set by <code className="font-mono">SERVER_PORT</code>).
      </p>
      <a href="https://github.com/debpalash/OpenGTM/blob/main/docs/plugins/README.md" target="_blank" rel="noopener noreferrer"
        className="text-xs text-[var(--gtm-accent)] underline-offset-4 hover:underline">Plugin documentation</a>
    </div>
  )
}
