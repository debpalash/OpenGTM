// The paged run list as TanStack Query keeps it: InfiniteData, one entry per
// loaded page, newest runs first. These pure helpers flatten it for display and
// patch it in place for live events, so the hooks and the event handler share
// one definition of "the list".

import type { InfiniteData } from "@tanstack/react-query"
import type { PluginRun, PluginRunPage } from "./plugin-api"

/** The first page has no cursor; later pages use the previous page's `next_cursor`. */
export type RunListData = InfiniteData<PluginRunPage, string | null>

/** `getNextPageParam`: undefined (no more pages) when the server sent no cursor. */
export const nextRunCursor = (last: PluginRunPage): string | undefined => last.next_cursor ?? undefined

/**
 * Every loaded run in list order. Keyset pages cannot overlap, but a run
 * inserted by a live event or a mutation while pages are loading can briefly
 * appear twice; the first (newest page) copy wins.
 */
export function flattenRuns(data: RunListData | undefined): PluginRun[] {
  if (!data) return []
  const seen = new Set<string>()
  const runs: PluginRun[] = []
  for (const page of data.pages) {
    for (const run of page.runs) {
      if (seen.has(run.id)) continue
      seen.add(run.id)
      runs.push(run)
    }
  }
  return runs
}

export function hasRun(data: RunListData | undefined, id: string): boolean {
  return !!data?.pages.some(page => page.runs.some(run => run.id === id))
}

/** Apply `fn` to every loaded run; page boundaries and cursors are untouched. */
export function mapRuns(data: RunListData | undefined, fn: (run: PluginRun) => PluginRun): RunListData | undefined {
  return data && { ...data, pages: data.pages.map(page => ({ ...page, runs: page.runs.map(fn) })) }
}

/** Put a new run at the top of the first page. The first page's cursor still names its old last row, so page 2 stays valid. */
export function prependRun(data: RunListData | undefined, run: PluginRun): RunListData | undefined {
  if (!data || data.pages.length === 0 || hasRun(data, run.id)) return data
  const [first, ...rest] = data.pages
  return { ...data, pages: [{ ...first, runs: [run, ...first.runs] }, ...rest] }
}

/** Replace a run in place, or prepend it when it is not loaded. */
export function upsertRun(data: RunListData | undefined, run: PluginRun): RunListData | undefined {
  return hasRun(data, run.id) ? mapRuns(data, r => r.id === run.id ? run : r) : prependRun(data, run)
}

/** How many rows from the end of the list the next page is requested. */
export const LOAD_MORE_THRESHOLD = 8

/**
 * Infinite scrolling: request the next page once the last rendered row is
 * within LOAD_MORE_THRESHOLD of the end of what is loaded. Never while a page
 * is in flight, and never when the server has no more.
 */
export function shouldLoadMore(args: { lastVisibleIndex: number; count: number; hasNextPage: boolean; busy: boolean }): boolean {
  const { lastVisibleIndex, count, hasNextPage, busy } = args
  return hasNextPage && !busy && count > 0 && lastVisibleIndex >= count - 1 - LOAD_MORE_THRESHOLD
}
