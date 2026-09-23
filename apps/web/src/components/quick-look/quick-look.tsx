/**
 * Quick Look — macOS Finder-style preview. Focus an item and press Space.
 *
 * Items are ordinary elements marked with `quickLookProps(payload)`: they
 * become focusable, keyboard-navigable (`data-nav-item`) and carry a plain-data
 * preview. Space opens the preview; ←/→ walk the items in DOM order (focus
 * follows); Space or Esc closes and returns focus to the item last shown.
 * Surfaces with their own cell navigation (the workbook grid) call `open()`
 * with a source instead.
 */
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { ArrowUpRight, ChevronLeft, ChevronRight } from "lucide-react"
import { Link } from "react-router-dom"

import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog"
import { Kbd } from "@/components/ui/kbd"
import { cn } from "@/lib/utils"

export type QuickLookField = { label: string; value: string; href?: string }
export type QuickLookPayload = {
  kind: string
  title: string
  subtitle?: string
  image?: { src: string; alt: string }
  /** A company domain: shown as the favicon when there is no image. */
  domain?: string
  fields?: QuickLookField[]
  actions?: { label: string; href: string; external?: boolean }[]
}
export type QuickLookSource = {
  count: number
  index: number
  get: (index: number) => QuickLookPayload | null
  /** Element to focus after closing on `index` (focus follows the preview). */
  elementAt?: (index: number) => HTMLElement | null
  onIndexChange?: (index: number) => void
}

type QuickLookApi = { open: (source: QuickLookSource) => void; isOpen: boolean }
const QuickLookContext = createContext<QuickLookApi | null>(null)

export function useQuickLook(): QuickLookApi {
  const api = useContext(QuickLookContext)
  if (!api) throw new Error("useQuickLook must be used inside <QuickLookProvider>")
  return api
}

/** Props that make an element a Quick Look item. Payload must be plain data. */
export function quickLookProps(payload: QuickLookPayload) {
  return { "data-quicklook": JSON.stringify(payload), "data-nav-item": "", tabIndex: 0 } as const
}

function visible(element: HTMLElement) {
  return element.offsetParent !== null || element.getClientRects().length > 0
}

/** Build a source from every visible Quick Look item, starting at `element`. */
export function sourceFromElement(element: HTMLElement): QuickLookSource | null {
  const items = Array.from(document.querySelectorAll<HTMLElement>("[data-quicklook]")).filter(visible)
  const index = items.indexOf(element)
  if (index < 0) return null
  return {
    count: items.length,
    index,
    get: i => {
      try { return JSON.parse(items[i]?.dataset.quicklook ?? "null") as QuickLookPayload } catch { return null }
    },
    elementAt: i => items[i] ?? null,
    onIndexChange: i => items[i]?.scrollIntoView({ block: "nearest" }),
  }
}

/** Only http(s) URLs from record data may become links or image sources. */
export function safeUrl(value: string | undefined | null): string | undefined {
  if (!value) return undefined
  try {
    const url = new URL(value, window.location.origin)
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : undefined
  } catch {
    return undefined
  }
}

function initials(title: string) {
  return title.split(/\s+/).filter(Boolean).slice(0, 2).map(word => word[0]?.toUpperCase()).join("") || "?"
}

function QuickLookView({ payload }: { payload: QuickLookPayload }) {
  const imageSrc = safeUrl(payload.image?.src)
  if (payload.image && imageSrc) {
    return (
      <div className="flex max-h-[70vh] min-h-64 items-center justify-center bg-[var(--t-background-secondary)]">
        <img src={imageSrc} alt={payload.image.alt} className="max-h-[70vh] max-w-full object-contain" />
      </div>
    )
  }
  return (
    <div className="grid gap-5 p-6">
      <div className="flex items-center gap-4">
        <div className="flex size-14 shrink-0 items-center justify-center overflow-hidden rounded-[var(--gtm-radius-tile)] bg-[var(--t-background-tertiary)] text-lg font-semibold text-muted-foreground shadow-[var(--gtm-shadow-card)]">
          {payload.domain
            ? <img src={`https://www.google.com/s2/favicons?domain=${encodeURIComponent(payload.domain)}&sz=64`} alt="" className="size-8" />
            : initials(payload.title)}
        </div>
        <div className="min-w-0">
          <p className="truncate text-xl font-semibold">{payload.title}</p>
          {payload.subtitle && <p className="truncate text-sm text-muted-foreground">{payload.subtitle}</p>}
        </div>
      </div>
      {payload.fields && payload.fields.length > 0 && (
        <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-6 gap-y-2 text-sm">
          {payload.fields.map(field => (
            <div key={field.label} className="contents">
              <dt className="text-muted-foreground">{field.label}</dt>
              <dd className="min-w-0 truncate">
                {safeUrl(field.href)
                  ? <a href={safeUrl(field.href)} target="_blank" rel="noreferrer" className="text-[var(--gtm-accent)] hover:underline">{field.value}</a>
                  : field.value}
              </dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  )
}

export function QuickLookProvider({ children }: { children: ReactNode }) {
  const [source, setSource] = useState<QuickLookSource | null>(null)
  const [index, setIndex] = useState(0)
  // Focus returns to the item last shown. It is resolved at close time: a
  // virtualized row may only exist after the preview moved to it.
  const opener = useRef<HTMLElement | null>(null)
  // Initial focus goes to the (non-activatable) title bar, as in Finder's Quick
  // Look. Focusing the first action instead let the key-up of the same Space
  // press activate it, closing the preview and navigating away.
  const titleBar = useRef<HTMLDivElement | null>(null)
  const current = useRef<{ source: QuickLookSource; index: number } | null>(null)
  const finalFocus = useCallback(() => {
    const state = current.current
    return (state && state.source.elementAt?.(state.index)) || opener.current || true
  }, [])

  const open = useCallback((next: QuickLookSource) => {
    if (next.count <= 0) return
    opener.current = document.activeElement as HTMLElement | null
    current.current = { source: next, index: next.index }
    setSource(next)
    setIndex(next.index)
  }, [])

  const move = useCallback((delta: number) => {
    if (!source) return
    const next = (index + delta + source.count) % source.count
    setIndex(next)
    current.current = { source, index: next }
    source.onIndexChange?.(next)
  }, [index, source])

  // Keys are handled on the window while open, so ←/→/Space work even in the
  // first moments before the dialog has moved focus into the panel.
  useEffect(() => {
    if (!source) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.metaKey || event.ctrlKey || event.altKey) return
      const target = event.target as HTMLElement | null
      if (target?.closest?.("input, textarea, [contenteditable=true]")) return
      if (event.key === "ArrowRight" || event.key === "ArrowDown") { event.preventDefault(); event.stopPropagation(); move(1) }
      else if (event.key === "ArrowLeft" || event.key === "ArrowUp") { event.preventDefault(); event.stopPropagation(); move(-1) }
      else if (event.key === " ") { event.preventDefault(); event.stopPropagation(); setSource(null) }
    }
    window.addEventListener("keydown", onKeyDown, true)
    return () => window.removeEventListener("keydown", onKeyDown, true)
  }, [move, source])

  const payload = source?.get(index) ?? null
  const api = useMemo(() => ({ open, isOpen: source !== null }), [open, source])

  return (
    <QuickLookContext.Provider value={api}>
      {children}
      <Dialog open={source !== null} onOpenChange={value => { if (!value) setSource(null) }}>
        <DialogContent
          showCloseButton={false}
          initialFocus={titleBar}
          finalFocus={finalFocus}
          data-quicklook-panel=""
          className="gap-0 overflow-hidden p-0 sm:max-w-2xl"
        >
          {payload && <>
            <div ref={titleBar} tabIndex={-1} className="flex h-11 items-center gap-2 border-b border-border px-3 outline-none">
              <span className="rounded-sm bg-[var(--t-background-transparent-medium)] px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">{payload.kind}</span>
              <DialogTitle className="min-w-0 flex-1 truncate text-sm font-semibold">{payload.title}</DialogTitle>
              <DialogDescription className="sr-only">Quick Look preview. Use the arrow keys to browse and Space to close.</DialogDescription>
              {payload.actions?.filter(action => action.href.startsWith("/") || safeUrl(action.href)).map(action => (
                <Button key={action.href} size="sm" variant="outline" nativeButton={false}
                  render={action.href.startsWith("/") && !action.external
                    ? <Link to={action.href} onClick={() => setSource(null)} />
                    : <a href={safeUrl(action.href)} target={action.external ? "_blank" : undefined} rel={action.external ? "noreferrer" : undefined} />}>
                  {action.label}{action.external && <ArrowUpRight aria-hidden="true" />}
                </Button>
              ))}
            </div>
            <QuickLookView payload={payload} />
            <div className="flex h-10 items-center justify-between gap-3 border-t border-border px-3 text-xs text-muted-foreground">
              <div className="flex items-center gap-1">
                <Button size="icon-xs" variant="ghost" aria-label="Previous item" disabled={(source?.count ?? 0) < 2} onClick={() => move(-1)}><ChevronLeft /></Button>
                <Button size="icon-xs" variant="ghost" aria-label="Next item" disabled={(source?.count ?? 0) < 2} onClick={() => move(1)}><ChevronRight /></Button>
                <span className={cn("tabular-nums", (source?.count ?? 0) < 2 && "sr-only")}>{index + 1} of {source?.count}</span>
              </div>
              <span className="hidden items-center gap-1.5 sm:flex"><Kbd>←</Kbd><Kbd>→</Kbd> browse <Kbd>Space</Kbd> close</span>
            </div>
          </>}
        </DialogContent>
      </Dialog>
    </QuickLookContext.Provider>
  )
}
