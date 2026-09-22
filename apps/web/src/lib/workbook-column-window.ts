export type ColumnWindowSegment =
  | { kind: "column"; index: number; width: number }
  | { kind: "spacer"; start: number; end: number; width: number }

/** Preserve absolute indexes and exact table width while skipping offscreen cells.
 * Retained indexes keep focused/editing cells mounted even outside the viewport.
 */
export function workbookColumnWindow(
  widths: readonly number[], scrollLeft: number, viewportWidth: number,
  retained: readonly number[] = [], overscanPx = 240,
): ColumnWindowSegment[] {
  if (widths.some(width => !Number.isFinite(width) || width <= 0)) throw new Error("Invalid column width")
  if (![scrollLeft, viewportWidth, overscanPx].every(Number.isFinite) || viewportWidth < 0 || overscanPx < 0) throw new Error("Invalid column viewport")
  const keep = new Set(retained)
  const left = Math.max(0, scrollLeft - overscanPx)
  const right = Math.max(0, scrollLeft) + viewportWidth + overscanPx
  const segments: ColumnWindowSegment[] = []
  let offset = 0
  widths.forEach((width, index) => {
    const visible = offset + width > left && offset < right
    if (visible || keep.has(index)) {
      segments.push({ kind: "column", index, width })
    } else {
      const last = segments.at(-1)
      if (last?.kind === "spacer") { last.end = index + 1; last.width += width }
      else segments.push({ kind: "spacer", start: index, end: index + 1, width })
    }
    offset += width
  })
  return segments
}
