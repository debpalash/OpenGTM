// Pointer coordinates are visual pixels; layout widths remain CSS pixels.
export function documentScale() {
  return Number.parseFloat(getComputedStyle(document.documentElement).zoom) || 1
}
