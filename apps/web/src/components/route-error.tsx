import { useEffect } from "react"
import { useRouteError } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { isStaleBuildError, reloadForNewBuild } from "@/lib/stale-build"

// Shown when a page throws while rendering, in place of the router's default
// developer screen. The message is kept so a bug report can quote it.
export function RouteError() {
  const error = useRouteError()
  const message = error instanceof Error ? error.message : typeof error === "string" ? error : "Unknown error"
  const stale = isStaleBuildError(error)
  // An open tab from before an upgrade: reload once to get the new version.
  useEffect(() => { if (stale) reloadForNewBuild() }, [stale])
  return (
    <main className="mx-auto flex min-h-svh max-w-lg flex-col items-start justify-center gap-4 p-6">
      <h1 className="text-xl font-semibold">{stale ? "OpenGTM was updated" : "This page could not be displayed"}</h1>
      <p className="text-sm text-muted-foreground">
        {stale
          ? "This tab is from before the update. Reload to get the new version. Your data is not affected."
          : "Something went wrong while loading it. Your data is not affected. Reload the page, or go back to the overview."}
      </p>
      <p role="alert" className="w-full rounded-md bg-muted p-3 font-mono text-xs break-words">{message}</p>
      <div className="flex gap-2">
        <Button onClick={() => window.location.reload()}>Reload page</Button>
        <Button variant="outline" onClick={() => window.location.assign("/overview")}>Go to overview</Button>
      </div>
    </main>
  )
}
