/**
 * Ink kit — parametric shapes for the koniu-inspired line-art style.
 *
 * Everything is a real outline (Bézier paths), so edges and bends are tuned
 * with numbers instead of redrawn by hand:
 *   limb()   tapered, bending tube with a rounded end (arms, legs, fingers)
 *   hand()   open mitten: palm + fanned fingers + thumb
 *   <Union>  draws several paths as ONE seamless outline: a double-width ink
 *            stroke underneath, paper fill on top (no seams where parts meet)
 *   <Card>   rounded card with a pin dot and a hard offset shadow
 *   ticks()  short emphasis strokes radiating from a point
 */
import type { ReactNode, SVGProps } from "react"
import "./ink.css"

export type Pt = readonly [number, number]

const add = (a: Pt, b: Pt): Pt => [a[0] + b[0], a[1] + b[1]]
const sub = (a: Pt, b: Pt): Pt => [a[0] - b[0], a[1] - b[1]]
const mul = (a: Pt, k: number): Pt => [a[0] * k, a[1] * k]
const length = (a: Pt) => Math.hypot(a[0], a[1])
const unit = (a: Pt): Pt => { const l = length(a) || 1; return [a[0] / l, a[1] / l] }
const perp = (a: Pt): Pt => [-a[1], a[0]]
const fmt = (p: Pt) => `${p[0].toFixed(1)} ${p[1].toFixed(1)}`
export const polar = (angleDeg: number, r = 1): Pt => {
  const a = (angleDeg * Math.PI) / 180
  return [Math.cos(a) * r, Math.sin(a) * r]
}

/**
 * Tapered tube from `a` (width w0) to `b` (width w1), bowed sideways by
 * `bend` (px, + bends toward the left of a→b), with a rounded end at `b`.
 * The start is flat: tuck it under the part it grows out of.
 */
export function limb(a: Pt, b: Pt, w0: number, w1: number, bend = 0): string {
  const d = unit(sub(b, a))
  const n = perp(d)
  const mid = add(add(a, mul(sub(b, a), 0.5)), mul(n, bend))
  const wm = (w0 + w1) / 2
  const a1 = add(a, mul(n, w0 / 2)), a2 = add(a, mul(n, -w0 / 2))
  const b1 = add(b, mul(n, w1 / 2)), b2 = add(b, mul(n, -w1 / 2))
  const c1 = add(mid, mul(n, wm / 2)), c2 = add(mid, mul(n, -wm / 2))
  const k = w1 * 0.66 // cubic handle length for a round end cap
  return `M${fmt(a1)} Q${fmt(c1)} ${fmt(b1)} C${fmt(add(b1, mul(d, k)))} ${fmt(add(b2, mul(d, k)))} ${fmt(b2)} Q${fmt(c2)} ${fmt(a2)} Z`
}

/** Ellipse as a path (so it can join a <Union>). */
export function oval(c: Pt, rx: number, ry: number, rotateDeg = 0): string {
  const k = 0.5523
  const ux = polar(rotateDeg, rx), uy = polar(rotateDeg + 90, ry)
  const p = (sx: number, sy: number) => add(c, add(mul(ux, sx), mul(uy, sy)))
  return `M${fmt(p(1, 0))} C${fmt(p(1, k))} ${fmt(p(k, 1))} ${fmt(p(0, 1))} C${fmt(p(-k, 1))} ${fmt(p(-1, k))} ${fmt(p(-1, 0))}` +
    ` C${fmt(p(-1, -k))} ${fmt(p(-k, -1))} ${fmt(p(0, -1))} C${fmt(p(k, -1))} ${fmt(p(1, -k))} ${fmt(p(1, 0))} Z`
}

/**
 * Open mitten hand at `c`, fingers pointing along `angle` (deg), palm radius
 * `size`. `spread` fans the four fingers; `thumb` is -1/1 for its side.
 */
export function hand(c: Pt, angle: number, size: number, spread = 16, thumb: 1 | -1 = 1): string[] {
  const paths = [oval(c, size * 0.95, size * 0.82, angle)]
  const fingerLen = size * 1.05
  for (let i = 0; i < 4; i++) {
    const a = angle + (i - 1.5) * spread
    const base = add(c, polar(a, size * 0.55))
    paths.push(limb(base, add(base, polar(a, fingerLen)), size * 0.44, size * 0.38, 0))
  }
  const ta = angle + thumb * 78
  const tb = add(c, polar(ta, size * 0.45))
  paths.push(limb(tb, add(tb, polar(ta - thumb * 18, size * 0.8)), size * 0.44, size * 0.38, 0))
  return paths
}

/** Several paths drawn as a single seamless ink outline. */
export function Union({ paths, className, ...rest }: { paths: string[] } & SVGProps<SVGGElement>) {
  return (
    <g className={className} {...rest}>
      <g className="ink-under">{paths.map((d, i) => <path key={i} d={d} />)}</g>
      <g className="ink-over">{paths.map((d, i) => <path key={i} d={d} />)}</g>
    </g>
  )
}

/** Rounded card with a pin dot and a hard offset shadow (koniu's objects). */
export function Card({ x, y, w, h, r = 8, pin = true, shadow = 4, children, ...rest }:
  { x: number; y: number; w: number; h: number; r?: number; pin?: boolean; shadow?: number; children?: ReactNode } & SVGProps<SVGGElement>) {
  return (
    <g {...rest}>
      <rect x={x + shadow * 0.6} y={y + shadow} width={w} height={h} rx={r} className="scene-ink" />
      <rect x={x} y={y} width={w} height={h} rx={r} className="scene-paper" />
      {pin && <circle cx={x + 8} cy={y + 8} r="2.3" className="scene-ink" />}
      {children}
    </g>
  )
}

/** Short emphasis strokes radiating from `c` between radii r0..r1. */
export function ticks(c: Pt, angles: number[], r0: number, r1: number): string {
  return angles.map(a => `M${fmt(add(c, polar(a, r0)))} L${fmt(add(c, polar(a, r1)))}`).join(" ")
}

/** Point along a→b at t, offset sideways by `side` (helper for placing props). */
export function along(a: Pt, b: Pt, t: number, side = 0): Pt {
  const d = sub(b, a)
  return add(add(a, mul(d, t)), mul(perp(unit(d)), side))
}
