// After an upgrade, a tab that was already open still points at page files
// from the old build, which no longer exist. Loading such a page fails; the
// fix is to reload once and pick up the new build.

const KEY = "gtm:stale-build-reload"
const MIN_GAP_MS = 30_000

export function isStaleBuildError(error: unknown): boolean {
  const message = error instanceof Error ? error.message : typeof error === "string" ? error : ""
  return /dynamically imported module|Importing a module script failed|error loading dynamically imported|Unable to preload CSS|ChunkLoadError/i.test(message)
}

/** Reloads the page unless it already did so moments ago. Returns whether it reloaded. */
export function reloadForNewBuild(now = Date.now()): boolean {
  try {
    const last = Number(sessionStorage.getItem(KEY) ?? 0)
    if (now - last < MIN_GAP_MS) return false
    sessionStorage.setItem(KEY, String(now))
  } catch {
    return false // without storage there is no loop guard, so do not reload automatically
  }
  window.location.reload()
  return true
}
