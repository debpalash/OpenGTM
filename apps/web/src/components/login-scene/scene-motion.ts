/**
 * Motion for the sign-in scene (anime.js v4, MIT). Loaded lazily by LoginScene
 * only on wide screens without reduced motion.
 *
 * Structure, so beats can be added or tuned independently:
 *   idle()      — breathing, bobbing, blinking (always running)
 *   story()     — the looping Find → Enrich → Act → reactions timeline
 *   shout()     — a one-off megaphone burst (also used on mascot click)
 *   pointer     — the mascot's face follows the cursor
 *   hover       — a lead hops and speaks when hovered
 * Parts are addressed by data-part / data-index in login-scene.tsx.
 */
import { animate, createScope, createTimeline, stagger, svg, utils } from "animejs"

export type SceneController = { pause: () => void; resume: () => void; destroy: () => void }

type Playable = { pause: () => unknown; play: () => unknown }

export function startScene(root: SVGSVGElement): SceneController {
  const q = <T extends Element = SVGElement>(selector: string) => Array.from(root.querySelectorAll<T>(selector))
  const one = (selector: string) => root.querySelector<SVGElement>(selector)
  const running: Playable[] = []
  const track = <T extends Playable>(animation: T) => { running.push(animation); return animation }
  const cleanups: (() => void)[] = []

  const scope = createScope({ root }).add(() => {
    const rows = q('[data-part="row"]')
    const checks = q('[data-part="check"]')
    const phases = q('[data-part="phase-label"]')
    const envelopes = q('[data-part="envelope"]')
    const bubbles = q('[data-part="bubble"]')
    const leads = q('[data-part="lead"]')
    const result = one('[data-part="result"]')
    const lens = one('[data-part="lens"]')
    const emphasis = one('[data-part="ticks"]')
    const megaphone = one('[data-part="megaphone"]')
    const shoutLines = one('[data-part="shout"]')
    const mascot = one('[data-part="mascot"]')
    const face = one('[data-part="face"]')
    const drawnChecks = checks.flatMap(check => svg.createDrawable(check))

    // Start from an empty stage (the markup's static state is the "finished" frame).
    utils.set(rows, { scaleX: 0 })
    utils.set(drawnChecks, { draw: "0 0" })
    utils.set(bubbles, { scale: 0, opacity: 0 })
    if (result) utils.set(result, { opacity: 0, translateY: 12 })
    utils.set(phases, { opacity: 0 })
    if (lens) utils.set(lens, { opacity: 0 })
    if (emphasis) utils.set(emphasis, { opacity: 0 })

    // ── idle ────────────────────────────────────────────────────────────
    if (mascot) track(animate(mascot, { translateY: [0, -5], duration: 1600, ease: "inOutSine", loop: true, alternate: true }))
    leads.forEach((lead, i) => {
      track(animate(lead, { translateY: [0, -4], duration: 1400 + i * 260, delay: i * 180, ease: "inOutSine", loop: true, alternate: true }))
    })
    track(animate(q('[data-part="eye"]'), {
      scaleY: [1, 0.12, 1], duration: 220, ease: "inOutQuad", loop: true, loopDelay: 3400, delay: stagger(90),
    }))

    // ── shout (reusable) ────────────────────────────────────────────────
    const shout = (at?: number) => {
      const tl = createTimeline({ autoplay: at === undefined })
      if (mascot) tl.add(mascot, { scaleY: [1, 0.94, 1], scaleX: [1, 1.04, 1], duration: 420, ease: "outQuad" }, 0)
      if (megaphone) tl.add(megaphone, { rotate: [0, -7, 3, 0], duration: 520, ease: "outElastic(1, .6)" }, 0)
      if (shoutLines) tl.add(shoutLines, { opacity: [0, 1, 0], scale: [0.8, 1.15], duration: 700, ease: "outQuad" }, 60)
      return tl
    }

    // ── flights: envelope i travels flight path i ───────────────────────
    // autoplay=false when a timeline will sync (own) the animation.
    const fly = (i: number, autoplay = true) => {
      const path = svg.createMotionPath(`#gtm-flight-${i}`)
      return animate(envelopes[i], {
        autoplay,
        translateX: path.translateX, translateY: path.translateY,
        opacity: [{ to: 1, duration: 120 }, { to: 1, duration: 1000 }, { to: 0, duration: 180 }],
        duration: 1300, ease: "inOutSine",
      })
    }
    const react = (i: number, autoplay = true) => {
      const tl = createTimeline({ autoplay })
      tl.add(leads[i], { translateY: [0, -14, 0], duration: 520, ease: "outQuad" }, 0)
        .add(bubbles[i], { scale: [0, 1], opacity: [0, 1], duration: 520, ease: "outBack(1.8)" }, 80)
      const wave = leads[i]?.querySelector('[data-part="wave"]')
      if (wave) tl.add(wave, { rotate: [0, -18, 10, -12, 0], duration: 900, ease: "inOutSine" }, 60)
      return tl
    }

    // ── story ───────────────────────────────────────────────────────────
    const story = track(createTimeline({ loop: true, loopDelay: 600 }))
    story
      // Find
      .add(phases[0], { opacity: [0, 1], translateY: [6, 0], duration: 300 }, 0)
      .add(rows, { scaleX: [0, 1], duration: 380, delay: stagger(170), ease: "outCubic" }, 150)
      .add(phases[0], { opacity: 0, duration: 200 }, 1250)
      // Enrich
      .add(phases[1], { opacity: [0, 1], translateY: [6, 0], duration: 300 }, 1350)
      .add(lens!, { opacity: [0, 1], duration: 200 }, 1350)
      .add(lens!, {
        // sweep over the four cards (lens starts on the first)
        translateX: [0, 110, 40, 150, 70], translateY: [0, 70, 140, 200, 210], duration: 1300, ease: "inOutSine",
      }, 1400)
      .add(drawnChecks, { draw: ["0 0", "0 1"], duration: 260, delay: stagger(260), ease: "outQuad" }, 1550)
      .add(lens!, { opacity: 0, duration: 200 }, 2700)
      .add(emphasis!, { opacity: [0, 1, 1, 0], scale: [0.92, 1.04, 1, 1], duration: 1100, ease: "outQuad" }, 1700)
      .add(phases[1], { opacity: 0, duration: 200 }, 2750)
      // Act
      .add(phases[2], { opacity: [0, 1], translateY: [6, 0], duration: 300 }, 2850)
      .sync(shout(0), 2950)
    envelopes.forEach((_, i) => { story.sync(fly(i, false), 3050 + i * 260) })
    // Reactions as each envelope lands (~1.3s after launch)
    envelopes.forEach((_, i) => { story.sync(react(i, false), 4250 + i * 260) })
    if (result) story.add(result, { opacity: [0, 1], translateY: [12, 0], duration: 500, ease: "outBack(1.4)" }, 4700)
    // Reset
    story
      .add(bubbles, { scale: 0, opacity: 0, duration: 260, delay: stagger(90), ease: "inQuad" }, 7600)
      .add(result!, { opacity: 0, translateY: 8, duration: 300 }, 7700)
      .add(phases[2], { opacity: 0, duration: 200 }, 7700)
      .add(drawnChecks, { draw: "0 0", duration: 200 }, 7900)
      .add(rows, { scaleX: 0, duration: 260, delay: stagger(60, { from: "last" }), ease: "inQuad" }, 8000)

    // ── interaction: click the mascot to shout ──────────────────────────
    let burst = 0
    const onMascotClick = () => {
      shout()
      const i = burst++ % envelopes.length
      fly(i).then(() => { react(i) })
    }
    mascot?.addEventListener("click", onMascotClick)
    cleanups.push(() => mascot?.removeEventListener("click", onMascotClick))

    // ── interaction: hover a lead ───────────────────────────────────────
    leads.forEach((lead, i) => {
      const onEnter = () => {
        react(i)
        animate(bubbles[i], { scale: 0, opacity: 0, duration: 260, delay: 1600, ease: "inQuad" })
      }
      lead.addEventListener("pointerenter", onEnter)
      cleanups.push(() => lead.removeEventListener("pointerenter", onEnter))
    })

    // ── interaction: the face follows the pointer ───────────────────────
    let frame = 0
    const onPointer = (event: PointerEvent) => {
      if (!face || !mascot) return
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        const box = mascot.getBoundingClientRect()
        const cx = box.left + box.width * 0.55
        const cy = box.top + box.height * 0.35
        const dx = event.clientX - cx
        const dy = event.clientY - cy
        const distance = Math.max(1, Math.hypot(dx, dy))
        const reach = Math.min(6, distance / 60)
        face.style.transform = `translate(${(dx / distance) * reach}px, ${(dy / distance) * reach}px)`
      })
    }
    window.addEventListener("pointermove", onPointer, { passive: true })
    cleanups.push(() => { window.removeEventListener("pointermove", onPointer); cancelAnimationFrame(frame) })
  })

  return {
    pause: () => running.forEach(animation => animation.pause()),
    resume: () => running.forEach(animation => animation.play()),
    destroy: () => { cleanups.forEach(cleanup => cleanup()); scope.revert() },
  }
}
