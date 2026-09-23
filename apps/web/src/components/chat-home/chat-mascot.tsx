/**
 * The OpenGTM mascot greeting on the empty chat, drawn with the same ink kit
 * as the sign-in scene. CSS drives the idle motion (bob, blink, wave), and the
 * eyes follow the pointer. Everything stops under Reduce Motion.
 */
import { useEffect, useRef } from "react"
import { Union, hand, limb, oval, ticks, type Pt } from "@/components/login-scene/ink"

const C: Pt = [110, 92]
const R = 52
const GROUND = 186

export function ChatMascot({ busy = false }: { busy?: boolean }) {
  const face = useRef<SVGGElement>(null)

  useEffect(() => {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
    let frame = 0
    const onMove = (event: PointerEvent) => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        const el = face.current
        if (!el) return
        const box = el.ownerSVGElement!.getBoundingClientRect()
        const dx = event.clientX - (box.left + box.width / 2)
        const dy = event.clientY - (box.top + box.height * 0.4)
        const d = Math.max(1, Math.hypot(dx, dy))
        const reach = Math.min(5, d / 70)
        el.style.transform = `translate(${(dx / d) * reach}px, ${(dy / d) * reach}px)`
      })
    }
    window.addEventListener("pointermove", onMove, { passive: true })
    return () => { window.removeEventListener("pointermove", onMove); cancelAnimationFrame(frame) }
  }, [])

  const [cx, cy] = C
  const legs = [
    limb([cx - 16, cy + R * 0.8], [cx - 20, GROUND - 4], 22, 17, -2),
    limb([cx + 12, cy + R * 0.82], [cx + 16, GROUND - 3], 22, 17, 2),
  ]
  const restArm = [
    limb([cx - R * 0.88, cy + 6], [cx - R * 1.1, cy + R * 0.95], 13, 11, 6),
    ...hand([cx - R * 1.13, cy + R * 1.1], 100, 8, 12, 1),
  ]
  const waveArm = [
    limb([cx + R * 0.8, cy + 4], [cx + R * 1.42, cy - R * 0.62], 13, 11, -8),
    ...hand([cx + R * 1.5, cy - R * 0.8], -62, 8.5, 15, -1),
  ]

  return (
    <svg viewBox="0 0 220 200" className="gtm-ink gtm-chat-mascot" data-busy={busy || undefined} aria-hidden="true">
      <defs>
        <filter id="gtm-pencil" x="-6%" y="-6%" width="112%" height="112%">
          <feTurbulence type="fractalNoise" baseFrequency="0.03" numOctaves="2" seed="4" result="warp" />
          <feDisplacementMap in="SourceGraphic" in2="warp" scale="1.2" xChannelSelector="R" yChannelSelector="G" />
        </filter>
      </defs>
      <ellipse cx={cx - 2} cy={GROUND} rx="60" ry="6" className="scene-ink gtm-chat-mascot-shadow" />
      <g className="scene-sketch gtm-chat-mascot-body">
        <Union paths={legs} />
        <ellipse cx={cx - 3} cy={cy + R * 0.94} rx="22" ry="6.5" className="scene-ink" />
        <Union paths={restArm} />
        <g className="gtm-chat-mascot-wave"><Union paths={waveArm} /></g>
        <circle cx={cx} cy={cy} r={R} className="scene-paper" />
        <g ref={face} className="gtm-chat-mascot-face">
          <ellipse cx={cx - 4} cy={cy - 14} rx="3.6" ry="4.6" className="scene-ink gtm-chat-mascot-eye" />
          <ellipse cx={cx + 16} cy={cy - 16} rx="3.6" ry="4.6" className="scene-ink gtm-chat-mascot-eye" />
          <path d={`M${cx - 3} ${cy + 2} Q${cx + 7} ${cy + 16} ${cx + 20} ${cy} Z`} className="scene-ink" />
          <path d={oval([cx - 16, cy + 2], 5, 3)} className="scene-tint" />
          <path d={oval([cx + 30, cy - 1], 5, 3)} className="scene-tint" />
        </g>
        <path d={ticks([cx + R * 1.5, cy - R * 0.8], [-150, -115, -80], 18, 26)} className="scene-line gtm-chat-mascot-ticks" />
      </g>
    </svg>
  )
}
