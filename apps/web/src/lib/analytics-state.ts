/**
 * Pure state derivation for one analytics data region (UI-01 / audit A01).
 * Every region is exactly one of: loading, error, empty, loaded. `stale` is set
 * when old data is still shown but the latest refetch failed.
 */
export type RegionKind = "loading" | "error" | "empty" | "loaded"
export interface RegionQuery<T = unknown> {
  data: T | undefined
  isError: boolean
  dataUpdatedAt?: number
}
export interface RegionState {
  kind: RegionKind
  stale: boolean
}

export function analyticsRegionState<T>(
  query: RegionQuery<T>,
  isEmpty: (data: T) => boolean = () => false
): RegionState {
  if (query.data === undefined) {
    // No data at all: an errored query is "unavailable", never zeros.
    return { kind: query.isError ? "error" : "loading", stale: false }
  }
  return {
    kind: isEmpty(query.data) ? "empty" : "loaded",
    stale: query.isError
  }
}

/** Freshness is the oldest successful load among regions that have data, or null. */
export function analyticsFreshness(
  queries: Array<Pick<RegionQuery, "data" | "dataUpdatedAt">>
): number | null {
  const times = queries
    .filter((q) => q.data !== undefined && q.dataUpdatedAt)
    .map((q) => q.dataUpdatedAt as number)
  return times.length ? Math.min(...times) : null
}
