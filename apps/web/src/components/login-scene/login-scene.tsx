/**
 * Sign-in backdrop: an interactive line-art story of what OpenGTM does, in a
 * koniu-inspired style (see ./ink.tsx for the shape kit).
 *
 * Left: the mascot works a tablet of workbook cards (Find → Enrich), then
 * shouts through a megaphone (Act). Envelopes arc over the sign-in card to the
 * right, where leads and a company react with speech bubbles while a verified
 * result card appears.
 *
 * One 1600×900 coordinate space so flights can cross it. Motion lives in
 * ./scene-motion.ts (anime.js, lazy). Parts are addressed by data-part.
 * Decorative (aria-hidden); a visible control pauses it (WCAG 2.2.2).
 */
import { useEffect, useRef, useState } from "react"
import { Pause, Play } from "lucide-react"

import { Card, Union, hand, limb, oval, ticks, type Pt } from "./ink"
import type { SceneController } from "./scene-motion"

// ── Composition constants ─────────────────────────────────────────────────
const GROUND = 796

// Mascot (left): body centre + radius; face looks right, megaphone up-right.
const M: Pt = [432, 648]
const MR = 74
// Megaphone: small end S, axis 45° up-right, MEGA_LEN long; mouth centre B.
const MEGA_S: Pt = [490, 640]
const MEGA_D: Pt = [Math.SQRT1_2, -Math.SQRT1_2]
const MEGA_N: Pt = [Math.SQRT1_2, Math.SQRT1_2]
const MEGA_LEN = 62
const MEGA_B: Pt = [MEGA_S[0] + MEGA_D[0] * MEGA_LEN, MEGA_S[1] + MEGA_D[1] * MEGA_LEN]
const at = (p: Pt, d: number, n: number): Pt => [p[0] + MEGA_D[0] * d + MEGA_N[0] * n, p[1] + MEGA_D[1] * d + MEGA_N[1] * n]
const xy = (p: Pt) => `${p[0].toFixed(1)} ${p[1].toFixed(1)}`
const FIST: Pt = at(MEGA_S, 20, 16)

const LAUNCH: Pt = at(MEGA_B, 14, 0)
/** Megaphone mouth → each lead, arcing over the sign-in card. */
const FLIGHTS = [
  `M${xy(LAUNCH)} C 700 -40, 1000 -40, 1146 640`,
  `M${xy(LAUNCH)} C 720 -60, 1080 -60, 1300 520`,
  `M${xy(LAUNCH)} C 740 -70, 1180 -70, 1440 650`,
]

const BUBBLES = [
  { text: "Loved the intro!", x: 1060, y: 578, w: 150 },
  { text: "Booked a call", x: 1226, y: 456, w: 132 },
  { text: "Tell me more", x: 1352, y: 594, w: 124 },
]

function Envelope({ index }: { index: number }) {
  return (
    <g data-part="envelope" data-index={index} opacity="0" className="scene-sketch">
      <rect x="-13" y="-7" width="30" height="21" rx="3" className="scene-ink" />
      <rect x="-15" y="-10" width="30" height="21" rx="3" className="scene-paper" />
      <path d="M-15 -8 L0 3 L15 -8" className="scene-line" />
    </g>
  )
}

function Bubble({ x, y, w, text, index }: { x: number; y: number; w: number; text: string; index: number }) {
  const h = 40
  return (
    <g data-part="bubble" data-index={index} className="scene-sketch scene-pop">
      <path d={`M${x + 10} ${y} H${x + w - 10} Q${x + w} ${y} ${x + w} ${y + 10} V${y + h - 10} Q${x + w} ${y + h} ${x + w - 10} ${y + h}
        H${x + 30} L${x + 16} ${y + h + 14} L${x + 18} ${y + h} H${x + 10} Q${x} ${y + h} ${x} ${y + h - 10} V${y + 10} Q${x} ${y} ${x + 10} ${y} Z`}
        className="scene-paper" />
      <text x={x + w / 2} y={y + 25} textAnchor="middle" className="scene-text">{text}</text>
    </g>
  )
}

/** Small round lead (koniu's little guys): body, tapered legs, face looking left. */
function RoundLead({ index, c, r, wave, tuft }: { index: number; c: Pt; r: number; wave?: boolean; tuft?: boolean }) {
  const [cx, cy] = c
  const legs = [
    limb([cx - r * 0.34, cy + r * 0.6], [cx - r * 0.42, GROUND - 6], r * 0.46, r * 0.36, -2),
    limb([cx + r * 0.22, cy + r * 0.62], [cx + r * 0.26, GROUND - 4], r * 0.46, r * 0.36, 2),
  ]
  // Arms grow from behind the body (drawn first; the body overlaps the shoulder).
  const shoulder: Pt = wave ? [cx - r * 0.7, cy - r * 0.1] : [cx - r * 0.72, cy + r * 0.3]
  const handAt: Pt = wave ? [cx - r * 1.55, cy - r * 0.95] : [cx - r * 1.2, cy + r * 1.0]
  const bumps = tuft ? [oval([cx + r * 0.62, cy - r * 0.72], r * 0.22, r * 0.22), oval([cx + r * 0.86, cy - r * 0.36], r * 0.2, r * 0.2)] : []
  return (
    <g data-part="lead" data-index={index} data-interactive="" className="scene-sketch">
      <Union paths={legs} />
      <ellipse cx={cx - r * 0.08} cy={cy + r * 0.86} rx={r * 0.6} ry={r * 0.16} className="scene-ink" />
      <g data-part="wave">
        <Union paths={[limb(shoulder, [handAt[0] + (wave ? 6 : 2), handAt[1] + (wave ? 10 : -8)], r * 0.34, r * 0.3, wave ? -8 : 6),
          ...hand(handAt, wave ? -118 : 100, r * 0.3, wave ? 18 : 12, wave ? -1 : 1)]} />
        {wave && <path d={ticks(handAt, [200, 215], r * 0.72, r * 0.95)} className="scene-line" />}
      </g>
      <Union paths={[oval(c, r, r), ...bumps]} />
      <ellipse data-part="eye" cx={cx - r * 0.44} cy={cy - r * 0.12} rx={r * 0.1} ry={r * 0.13} className="scene-ink" />
      <ellipse data-part="eye" cx={cx - r * 0.08} cy={cy - r * 0.16} rx={r * 0.1} ry={r * 0.13} className="scene-ink" />
      <path d={`M${cx - r * 0.44} ${cy + r * 0.14} Q${cx - r * 0.26} ${cy + r * 0.32} ${cx - r * 0.06} ${cy + r * 0.1}`} className="scene-line" />
    </g>
  )
}

export function LoginScene() {
  const root = useRef<SVGSVGElement | null>(null)
  const controller = useRef<SceneController | null>(null)
  const [paused, setPaused] = useState(false)
  const [animated, setAnimated] = useState(false)

  useEffect(() => {
    const svg = root.current
    if (!svg) return
    const wide = window.matchMedia("(min-width: 1180px)")
    const reduce = window.matchMedia("(prefers-reduced-motion: reduce)")
    // Skip the animation library entirely when the scene is hidden or motion is reduced.
    if (!wide.matches || reduce.matches) return
    let cancelled = false
    void import("./scene-motion").then(({ startScene }) => {
      if (cancelled) return
      controller.current = startScene(svg)
      setAnimated(true)
    })
    return () => {
      cancelled = true
      controller.current?.destroy()
      controller.current = null
    }
  }, [])

  const togglePause = () => {
    const next = !paused
    setPaused(next)
    if (next) controller.current?.pause()
    else controller.current?.resume()
  }

  // Mascot geometry, derived from the constants so proportions stay tunable.
  const [mx, my] = M
  const mascotLegs = [
    limb([mx - 22, my + MR * 0.8], [mx - 28, GROUND - 6], 32, 24, -3),
    limb([mx + 16, my + MR * 0.82], [mx + 22, GROUND - 4], 32, 24, 3),
  ]
  const hangingArm = [
    limb([mx - MR * 0.9, my + 8], [mx - MR * 1.08, my + MR * 1.06], 18, 15, 8),
    ...hand([mx - MR * 1.1, my + MR * 1.24], 98, 11, 13, 1),
  ]
  const megaArm = limb([mx + MR * 0.82, my + 20], FIST, 17, 15, -6)

  return (
    <div className="gtm-login-scene gtm-ink pointer-events-none absolute inset-0 hidden min-[1180px]:block" data-animated={animated || undefined}>
      <svg ref={root} viewBox="0 0 1600 900" preserveAspectRatio="xMidYMax slice" className="size-full" aria-hidden="true">
        <defs>
          {FLIGHTS.map((d, i) => <path key={i} id={`gtm-flight-${i}`} data-part="flight" data-index={i} d={d} fill="none" />)}
          {/* Ink: a faint hand-drawn wobble; lines stay clean, not grainy. */}
          <filter id="gtm-pencil" x="-6%" y="-6%" width="112%" height="112%">
            <feTurbulence type="fractalNoise" baseFrequency="0.03" numOctaves="2" seed="4" result="warp" />
            <feDisplacementMap in="SourceGraphic" in2="warp" scale="1.6" xChannelSelector="R" yChannelSelector="G" />
          </filter>
        </defs>

        {/* ── Left: tablet of workbook cards (Find → Enrich) ── */}
        <g data-part="window" className="scene-sketch">
          <rect x="96" y="326" width="384" height="454" rx="30" className="scene-ink" transform="translate(4 6)" />
          <rect x="96" y="326" width="384" height="454" rx="30" className="scene-paper" />
          <rect x="120" y="346" width="340" height="414" rx="12" className="scene-line scene-thin" />
          <circle cx="108" cy="553" r="5" className="scene-line scene-thin" />
          <path d="M206 356 V748 M392 356 V748" className="scene-line scene-thin" />
          {[
            { x: 146, y: 390, w: 176, h: 58 },
            { x: 236, y: 474, w: 190, h: 46 },
            { x: 140, y: 548, w: 214, h: 46 },
            { x: 232, y: 622, w: 150, h: 40 },
          ].map((card, i) => (
            <g key={i}>
              <Card data-part="row" data-index={i} x={card.x} y={card.y} w={card.w} h={card.h} />
              <path data-part="check" data-index={i}
                d={`M${card.x + card.w - 30} ${card.y + card.h / 2} l6 6 l12 -12`} className="scene-line scene-accent-line" />
            </g>
          ))}
          <path data-part="ticks" d={ticks([234, 419], [-160, -120, -90, -60, -25, 20, 160, 200], 108, 124)} className="scene-line" opacity="0" />
          <g data-part="lens">
            <circle cx="190" cy="430" r="13" className="scene-paper" />
            <path d="M199 439 L211 451" className="scene-line scene-thick" />
          </g>
        </g>
        <g className="scene-phase">
          {["Finding leads…", "Enriching contacts…", "Reaching out…"].map((label, i) => (
            <text key={label} data-part="phase-label" data-index={i} x="288" y="306" textAnchor="middle" className="scene-caption" opacity={i === 2 ? 1 : 0}>{label}</text>
          ))}
        </g>

        {/* ── Left: mascot in front of the tablet ── */}
        <ellipse cx={mx - 2} cy={GROUND} rx="86" ry="8" className="scene-ink" />
        <g data-part="mascot" data-interactive="" className="scene-sketch">
          <Union paths={mascotLegs} />
          <ellipse cx={mx - 4} cy={my + MR * 0.94} rx="30" ry="9" className="scene-ink" />
          <Union paths={hangingArm} />
          <circle cx={mx} cy={my} r={MR} className="scene-paper" />
          <g data-part="face">
            <ellipse data-part="eye" cx={mx + 26} cy={my - 22} rx="4.6" ry="5.8" className="scene-ink" />
            <ellipse data-part="eye" cx={mx + 50} cy={my - 26} rx="4.6" ry="5.8" className="scene-ink" />
            <path data-part="mouth" d={`M${mx + 28} ${my - 4} Q${mx + 40} ${my + 14} ${mx + 56} ${my - 8} Z`} className="scene-ink" />
          </g>
          <g data-part="megaphone">
            <Union paths={[megaArm]} />
            <path d={`M${xy(at(MEGA_S, 0, 9))} L${xy(at(MEGA_B, 0, 31))} L${xy(at(MEGA_B, 0, -31))} L${xy(at(MEGA_S, 0, -9))} Z`} className="scene-paper" />
            <path d={`M${xy(at(MEGA_S, 16, 13))} L${xy(at(MEGA_S, 16, -13))}`} className="scene-line" />
            <ellipse cx={MEGA_B[0]} cy={MEGA_B[1]} rx="10" ry="31" transform={`rotate(-45 ${xy(MEGA_B)})`} className="scene-paper" />
            <ellipse cx={MEGA_B[0] + 1.5} cy={MEGA_B[1] - 1.5} rx="5" ry="17" transform={`rotate(-45 ${xy(MEGA_B)})`} className="scene-line scene-thin" />
            <Union paths={[oval(FIST, 16, 14, -30), oval([FIST[0] - 13, FIST[1] - 6], 6, 5.5), oval([FIST[0] - 14, FIST[1] + 3], 6, 5.5), oval([FIST[0] - 11, FIST[1] + 11], 5.5, 5)]} />
            <path d={`M${FIST[0] - 6} ${FIST[1] - 9} q7 1 9 7`} className="scene-line scene-thin" />
          </g>
          <path data-part="shout" d={ticks(MEGA_B, [-95, -70, -45, -20, 5], 44, 60)} className="scene-line" opacity="0" />
        </g>

        {/* ── Envelopes in flight ── */}
        {FLIGHTS.map((_, i) => <Envelope key={i} index={i} />)}

        {/* ── Right: result card ── */}
        <Card data-part="result" className="scene-sketch scene-pop" x={1110} y={356} w={240} h={76} r={14}>
          <rect x="1126" y="374" width="40" height="40" rx="10" className="scene-tint" />
          <path d="M1136 402 L1146 386 L1156 402 Z" className="scene-line scene-thin" />
          <text x="1178" y="390" className="scene-title">Acme Robotics</text>
          <text x="1178" y="412" className="scene-caption">Verified · Fit score 92</text>
          <circle cx="1326" cy="394" r="11" className="scene-accent-fill" />
          <path d="M1320 394 L1324 398 L1332 389" className="scene-line scene-on-accent" />
        </Card>

        {/* ── Right: leads and a company ── */}
        <ellipse cx="1146" cy={GROUND} rx="52" ry="6" className="scene-ink" />
        <RoundLead index={0} c={[1150, 712]} r={40} wave />
        <ellipse cx="1302" cy={GROUND} rx="66" ry="6" className="scene-ink" />
        <g data-part="lead" data-index="1" data-interactive="" className="scene-sketch">
          <rect x="1254" y="548" width="96" height="242" rx="18" className="scene-ink" transform="translate(4 5)" />
          <rect x="1254" y="548" width="96" height="242" rx="18" className="scene-paper" />
          {[0, 1, 2].map(row => [0, 1].map(col => (
            <Card key={`${row}-${col}`} x={1270 + col * 36} y={640 + row * 42} w={28} h={30} r={6} shadow={3} />
          )))}
          <ellipse data-part="eye" cx="1286" cy="590" rx="4.4" ry="5.4" className="scene-ink" />
          <ellipse data-part="eye" cx="1312" cy="588" rx="4.4" ry="5.4" className="scene-ink" />
          <path d="M1288 608 Q1300 620 1314 606" className="scene-line" />
          <path d="M1302 548 V514" className="scene-line" />
          <path d="M1302 514 H1330 L1323 522 L1330 530 H1302 Z" className="scene-accent-fill" />
        </g>
        <ellipse cx="1438" cy={GROUND} rx="46" ry="6" className="scene-ink" />
        <RoundLead index={2} c={[1444, 726]} r={34} tuft />

        {BUBBLES.map((bubble, i) => <Bubble key={i} index={i} {...bubble} />)}
      </svg>

      {animated && (
        <button type="button" onClick={togglePause}
          className="pointer-events-auto absolute right-5 bottom-5 flex size-8 items-center justify-center rounded-full bg-white/85 text-[#1d1d1f] shadow-[var(--gtm-control-shadow)] outline-none [corner-shape:round] transition-colors hover:bg-white focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] [&_svg]:size-3.5"
          aria-label={paused ? "Play background animation" : "Pause background animation"} aria-pressed={paused}>
          {paused ? <Play /> : <Pause />}
        </button>
      )}
    </div>
  )
}
