/**
 * Sign-in backdrop: an interactive, line-art SVG story of what OpenGTM does.
 *
 * Left: the mascot works the platform (Find → Enrich → Act) and shouts
 * through a megaphone. Envelopes arc over the sign-in card to the right,
 * where leads and a company react (speech bubbles, a verified result card).
 *
 * The whole scene shares one 1600×900 coordinate space so flights can cross
 * it. Animation lives in ./scene-motion.ts (anime.js, loaded lazily); this
 * file is only markup, so new beats can be added by adding parts + a step.
 * Decorative (aria-hidden); a visible control pauses it (WCAG 2.2.2).
 */
import { useEffect, useRef, useState } from "react"
import { Pause, Play } from "lucide-react"

import type { SceneController } from "./scene-motion"

const LEADS = [
  { id: 0, bubble: "Loved the intro!", bubbleX: 1062, bubbleY: 590, bubbleW: 150 },
  { id: 1, bubble: "Booked a call", bubbleX: 1226, bubbleY: 492, bubbleW: 132 },
  { id: 2, bubble: "Tell me more", bubbleX: 1346, bubbleY: 580, bubbleW: 124 },
] as const

/** Paths the envelopes fly along: megaphone mouth → each lead, arcing over the card. */
const FLIGHTS = [
  "M372 526 C 600 0, 1000 0, 1150 600",
  "M372 526 C 620 -40, 1060 -40, 1300 540",
  "M372 526 C 640 -50, 1150 -50, 1440 612",
]

function Envelope({ index }: { index: number }) {
  return (
    <g data-part="envelope" data-index={index} opacity="0">
      <rect x="-15" y="-10" width="30" height="21" rx="3" className="scene-paper" />
      <path d="M-15 -8 L0 3 L15 -8" className="scene-line" />
    </g>
  )
}

function Bubble({ x, y, w, text, index }: { x: number; y: number; w: number; text: string; index: number }) {
  return (
    <g data-part="bubble" data-index={index} className="scene-pop">
      <path d={`M${x + 12} ${y} H${x + w - 12} Q${x + w} ${y} ${x + w} ${y + 12} V${y + 26} Q${x + w} ${y + 38} ${x + w - 12} ${y + 38}
        H${x + 34} L${x + 22} ${y + 50} L${x + 24} ${y + 38} H${x + 12} Q${x} ${y + 38} ${x} ${y + 26} V${y + 12} Q${x} ${y} ${x + 12} ${y} Z`}
        className="scene-paper" />
      <text x={x + w / 2} y={y + 24} textAnchor="middle" className="scene-text">{text}</text>
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

  return (
    <div className="gtm-login-scene pointer-events-none absolute inset-0 hidden min-[1180px]:block" data-animated={animated || undefined}>
      <svg ref={root} viewBox="0 0 1600 900" preserveAspectRatio="xMidYMax slice" className="size-full" aria-hidden="true">
        <defs>
          {FLIGHTS.map((d, i) => <path key={i} id={`gtm-flight-${i}`} data-part="flight" data-index={i} d={d} fill="none" />)}
        </defs>

        {/* ground shadows */}
        <g className="scene-shadow">
          <ellipse cx="262" cy="806" rx="120" ry="10" />
          <ellipse cx="482" cy="806" rx="100" ry="8" />
          <ellipse cx="1150" cy="806" rx="70" ry="8" />
          <ellipse cx="1300" cy="806" rx="70" ry="8" />
          <ellipse cx="1440" cy="806" rx="54" ry="7" />
        </g>

        {/* ── Left: podium ── */}
        <g>
          <path d="M180 720 H320 V800 H180 Z" className="scene-paper" />
          <path d="M180 720 L206 702 H346 L320 720 Z" className="scene-paper" />
          <path d="M320 720 L346 702 V782 L320 800 Z" className="scene-paper" />
          <path d="M326 742 H334 V756 H340 V770" className="scene-line" />
        </g>

        {/* ── Left: mascot ── */}
        <g data-part="mascot" data-interactive="" className="scene-mascot">
          <rect x="236" y="636" width="18" height="64" rx="9" className="scene-paper" />
          <rect x="262" y="636" width="18" height="64" rx="9" className="scene-paper" />
          <path d="M232 700 Q244 692 254 700" className="scene-line" />
          <path d="M258 700 Q270 692 282 700" className="scene-line" />
          <path d="M326 598 Q356 640 350 684" className="scene-line" />
          <circle cx="350" cy="690" r="7" className="scene-paper" />
          <circle cx="258" cy="585" r="72" className="scene-paper" />
          <path d="M214 640 Q258 668 302 640" className="scene-line scene-thick" />
          <g data-part="face">
            <ellipse data-part="eye" cx="282" cy="570" rx="4.5" ry="5.5" className="scene-ink" />
            <ellipse data-part="eye" cx="306" cy="566" rx="4.5" ry="5.5" className="scene-ink" />
            <path data-part="mouth" d="M284 592 Q296 610 310 588 Z" className="scene-ink" />
          </g>
          {/* megaphone + fist */}
          <g data-part="megaphone">
            <path d="M302 598 Q312 592 355.3 576.9" className="scene-line" />
            <path d="M346.7 561.9 L355.3 572.9" className="scene-line scene-thick" />
            <path d="M327.5 573.1 L389.3 551.5 L352.4 504.2 L316.5 558.9 Z" className="scene-paper" />
            <ellipse cx="370.9" cy="527.8" rx="11" ry="30" transform="rotate(-38.0 370.9 527.8)" className="scene-paper" />
            <ellipse cx="370.9" cy="527.8" rx="6" ry="17" transform="rotate(-38.0 370.9 527.8)" className="scene-line" />
            <circle cx="355.3" cy="572.9" r="11" className="scene-paper" />
            <path d="M349.3 569.9 H359.3 M349.3 574.9 H359.3" className="scene-line" />
          </g>
          <g data-part="shout" opacity="0">
            <path d="M362.5 491.2 L375.1 481.3" className="scene-line" />
            <path d="M374.8 506.9 L387.5 497.1" className="scene-line" />
            <path d="M389.6 525.9 L402.2 516.0" className="scene-line" />
            <path d="M401.9 541.6 L414.5 531.8" className="scene-line" />
          </g>
        </g>

        {/* ── Left: the OpenGTM window (Find → Enrich) ── */}
        <g data-part="window">
          <rect x="392" y="612" width="180" height="150" rx="12" className="scene-paper" />
          <path d="M392 636 H572" className="scene-line" />
          <circle cx="408" cy="624" r="3.6" fill="#ff5f57" />
          <circle cx="420" cy="624" r="3.6" fill="#febc2e" />
          <circle cx="432" cy="624" r="3.6" fill="#28c840" />
          <text x="512" y="628" textAnchor="middle" className="scene-caption">OpenGTM</text>
          {[652, 678, 704, 730].map((y, i) => (
            <g key={y}>
              <rect data-part="row" data-index={i} x="406" y={y} width="120" height="12" rx="5" className="scene-tint" />
              <path data-part="check" data-index={i} d={`M540 ${y + 6} L545 ${y + 11} L556 ${y + 1}`} className="scene-line scene-accent-line" />
            </g>
          ))}
          <g data-part="lens">
            <circle cx="430" cy="690" r="10" className="scene-paper" />
            <path d="M437 697 L446 706" className="scene-line scene-thick" />
          </g>
        </g>
        <g data-part="phase" className="scene-phase">
          {["Finding leads…", "Enriching contacts…", "Reaching out…"].map((label, i) => (
            <text key={label} data-part="phase-label" data-index={i} x="482" y="596" textAnchor="middle" className="scene-caption" opacity={i === 2 ? 1 : 0}>{label}</text>
          ))}
        </g>

        {/* ── Envelopes in flight ── */}
        {FLIGHTS.map((_, i) => <Envelope key={i} index={i} />)}

        {/* ── Right: result card ── */}
        <g data-part="result" className="scene-pop">
          <rect x="1110" y="360" width="236" height="72" rx="14" className="scene-paper" />
          <rect x="1124" y="376" width="40" height="40" rx="10" className="scene-tint" />
          <path d="M1134 402 L1144 386 L1154 402 Z" className="scene-line" />
          <text x="1176" y="392" className="scene-title">Acme Robotics</text>
          <text x="1176" y="414" className="scene-caption">Verified · Fit score 92</text>
          <circle cx="1322" cy="396" r="11" className="scene-accent-fill" />
          <path d="M1316 396 L1320 400 L1328 391" className="scene-line scene-on-accent" />
        </g>

        {/* ── Right: leads ── */}
        <g data-part="lead" data-index="0" data-interactive="" className="scene-lead">
          <path d="M1110 760 L1126 748 H1206 L1190 760 Z" className="scene-paper" />
          <path d="M1110 760 H1190 V800 H1110 Z" className="scene-paper" />
          <rect x="1136" y="712" width="12" height="40" rx="6" className="scene-paper" />
          <rect x="1154" y="712" width="12" height="40" rx="6" className="scene-paper" />
          <circle cx="1151" cy="690" r="36" className="scene-paper" />
          <ellipse data-part="eye" cx="1136" cy="684" rx="3.8" ry="4.6" className="scene-ink" />
          <ellipse data-part="eye" cx="1154" cy="682" rx="3.8" ry="4.6" className="scene-ink" />
          <path d="M1136 698 Q1146 708 1156 697" className="scene-line" />
          <path data-part="wave" d="M1184 690 Q1204 676 1206 654" className="scene-line" />
        </g>
        <g data-part="lead" data-index="1" data-interactive="" className="scene-lead">
          <rect x="1256" y="560" width="92" height="240" rx="14" className="scene-paper" />
          {[0, 1, 2].map(row => [0, 1, 2].map(col => (
            <rect key={`${row}-${col}`} x={1270 + col * 24} y={650 + row * 34} width="16" height="20" rx="4" className="scene-tint" />
          )))}
          <ellipse data-part="eye" cx="1288" cy="596" rx="4.2" ry="5" className="scene-ink" />
          <ellipse data-part="eye" cx="1316" cy="596" rx="4.2" ry="5" className="scene-ink" />
          <path d="M1290 614 Q1302 626 1314 613" className="scene-line" />
          <path d="M1302 560 V530" className="scene-line" />
          <path d="M1302 530 H1328 L1322 538 L1328 546 H1302" className="scene-accent-fill" />
        </g>
        <g data-part="lead" data-index="2" data-interactive="" className="scene-lead">
          <rect x="1422" y="736" width="12" height="62" rx="6" className="scene-paper" />
          <rect x="1446" y="736" width="12" height="62" rx="6" className="scene-paper" />
          <rect x="1408" y="652" width="64" height="92" rx="28" className="scene-paper" />
          <ellipse data-part="eye" cx="1426" cy="690" rx="3.8" ry="4.6" className="scene-ink" />
          <ellipse data-part="eye" cx="1446" cy="688" rx="3.8" ry="4.6" className="scene-ink" />
          <path d="M1426 706 Q1436 716 1448 704" className="scene-line" />
          <path data-part="wave" d="M1408 700 Q1388 684 1390 662" className="scene-line" />
        </g>
        {LEADS.map(lead => (
          <Bubble key={lead.id} index={lead.id} x={lead.bubbleX} y={lead.bubbleY} w={lead.bubbleW} text={lead.bubble} />
        ))}
      </svg>

      {animated && (
        <button type="button" onClick={togglePause}
          className="pointer-events-auto absolute right-5 bottom-5 flex size-8 items-center justify-center rounded-full bg-white/85 text-[#1d1d1f] shadow-[var(--gtm-control-shadow)] outline-none [corner-shape:round] transition-colors hover:bg-white focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] dark:bg-white/10 dark:text-white dark:hover:bg-white/20 [&_svg]:size-3.5"
          aria-label={paused ? "Play background animation" : "Pause background animation"} aria-pressed={paused}>
          {paused ? <Play /> : <Pause />}
        </button>
      )}
    </div>
  )
}
