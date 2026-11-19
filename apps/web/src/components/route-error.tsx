import { useRouteError } from "react-router-dom"
import { Button } from "@/components/ui/button"

// Shown when a page throws while rendering, in place of the router's default
// developer screen. The message is kept so a bug report can quote it.
export function RouteError() {
  const error = useRouteError()
  const message = error instanceof Error ? error.message : typeof error === "string" ? error : "Unknown error"
  return (
    <main className="mx-auto flex min-h-svh max-w-lg flex-col items-start justify-center gap-4 p-6">
      <h1 className="text-xl font-semibold">This page could not be displayed</h1>
      <p className="text-sm text-muted-foreground">
        Something went wrong while loading it. Your data is not affected. Reload the page, or go back to the overview.
      </p>
      <p role="alert" className="w-full rounded-md border bg-muted/40 p-3 font-mono text-xs break-words">{message}</p>
      <div className="flex gap-2">
        <Button onClick={() => window.location.reload()}>Reload page</Button>
        <Button variant="outline" onClick={() => window.location.assign("/overview")}>Go to overview</Button>
      </div>
    </main>
  )
}
