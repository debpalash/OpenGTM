/**
 * Site-wide keyboard shortcuts (Linear/macOS style) and the ? help overlay.
 *
 * Shortcuts never fire while typing, while a dialog is open, or with
 * ⌘/Ctrl/Alt held (those belong to the browser and to ⌘K / ⌘B).
 */
import { useCallback, useEffect, useRef, useState } from "react"
import { useNavigate } from "react-router-dom"

import { ALL_NAVIGATION, OPEN_SHORTCUTS_EVENT } from "@/components/app-shell/navigation"
import { sourceFromElement, useQuickLook } from "@/components/quick-look/quick-look"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Kbd } from "@/components/ui/kbd"
import { useSidebar } from "@/components/ui/sidebar"

const SEQUENCE_MS = 1200

export const SHORTCUT_GROUPS: { title: string; items: { keys: string[]; label: string }[] }[] = [
  { title: "General", items: [
    { keys: ["⌘", "K"], label: "Command palette" },
    { keys: ["?"], label: "Keyboard shortcuts" },
    { keys: ["/"], label: "Search this page" },
    { keys: ["["], label: "Toggle sidebar" },
    { keys: ["⇧", "T"], label: "Toggle light / dark" },
  ] },
  { title: "Lists", items: [
    { keys: ["J"], label: "Next item" },
    { keys: ["K"], label: "Previous item" },
    { keys: ["↵"], label: "Open item" },
    { keys: ["X"], label: "Select item" },
    { keys: ["Space"], label: "Quick Look" },
  ] },
  { title: "Quick Look", items: [
    { keys: ["←", "→"], label: "Previous / next item" },
    { keys: ["Space"], label: "Close" },
  ] },
  { title: "Go to", items: ALL_NAVIGATION.map(item => ({ keys: ["G", item.key.toUpperCase()], label: item.label })) },
]

function isTyping(target: EventTarget | null) {
  const element = target as HTMLElement | null
  if (!element?.closest) return false
  return Boolean(element.closest(
    "input, textarea, select, [contenteditable=''], [contenteditable=true], [role=textbox], [role=combobox]"))
}

function dialogOpen() {
  return Boolean(document.querySelector('[role="dialog"][data-open], [role="alertdialog"][data-open]'))
}

function navItems(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>("[data-nav-item]"))
    .filter(element => element.offsetParent !== null || element.getClientRects().length > 0)
}

function focusSearch(): boolean {
  const candidates = Array.from(document.querySelectorAll<HTMLInputElement>(
    'main input[data-shortcut-search], main input[type="search"], main input[placeholder*="Search" i], main input[placeholder*="Filter" i], ' +
    'input[data-shortcut-search], input[type="search"], input[placeholder*="Search" i], input[placeholder*="Filter" i]'))
  const target = candidates.find(input => !input.disabled && (input.offsetParent !== null))
  if (!target) return false
  target.focus()
  target.select()
  return true
}

export function KeyboardLayer() {
  const navigate = useNavigate()
  const { toggleSidebar } = useSidebar()
  const quickLook = useQuickLook()
  const [helpOpen, setHelpOpen] = useState(false)
  const pendingG = useRef<number | null>(null)

  const clearSequence = useCallback(() => {
    if (pendingG.current !== null) window.clearTimeout(pendingG.current)
    pendingG.current = null
  }, [])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return
      if (isTyping(event.target) || helpOpen || quickLook.isOpen || dialogOpen()) return
      const key = event.key

      // g-sequences: g then a section key.
      if (pendingG.current !== null) {
        clearSequence()
        const destination = ALL_NAVIGATION.find(item => item.key === key.toLowerCase())
        if (destination) {
          event.preventDefault()
          navigate(destination.to)
        }
        return
      }
      if (key === "g" && !event.shiftKey) {
        pendingG.current = window.setTimeout(clearSequence, SEQUENCE_MS)
        return
      }

      if (key === "?") { event.preventDefault(); setHelpOpen(true); return }
      if (key === "/") { if (focusSearch()) event.preventDefault(); return }
      if (key === "[") { event.preventDefault(); toggleSidebar(); return }
      if (key === "T" && event.shiftKey) {
        event.preventDefault()
        const dark = document.documentElement.classList.contains("dark")
        ;(window as unknown as { OpenGTMTheme?: { setPreference: (value: string) => void } })
          .OpenGTMTheme?.setPreference(dark ? "light" : "dark")
        return
      }

      // Lists: j/k (and ↑/↓ once an item has focus) move between items.
      const active = document.activeElement as HTMLElement | null
      // Surfaces with their own arrow-key model (grids, menus, tabs) keep it.
      if (active?.closest('[role="grid"], [role="menu"], [role="listbox"], [role="tablist"], [role="tree"]')) return
      const current = active?.closest<HTMLElement>("[data-nav-item]") ?? null
      const forward = key === "j" || (key === "ArrowDown" && current)
      const backward = key === "k" || (key === "ArrowUp" && current)
      if (forward || backward) {
        const items = navItems()
        if (!items.length) return
        event.preventDefault()
        const at = current ? items.indexOf(current) : -1
        const next = at < 0 ? (forward ? 0 : items.length - 1) : Math.min(items.length - 1, Math.max(0, at + (forward ? 1 : -1)))
        items[next].focus()
        items[next].scrollIntoView({ block: "nearest" })
        return
      }
      if (!current) return
      if (key === " ") {
        const source = current.hasAttribute("data-quicklook") ? sourceFromElement(current) : null
        if (source) { event.preventDefault(); quickLook.open(source) }
        return
      }
      if (key === "Enter" && current === active) {
        // Rows that open on click (data-nav-activate) are clicked; other rows
        // follow their primary link/action.
        if (current.hasAttribute("data-nav-activate")) { event.preventDefault(); current.click(); return }
        const primary = current.matches("a, button") ? null
          : current.querySelector<HTMLElement>("[data-nav-primary], a[href], button:not([disabled])")
        if (primary) { event.preventDefault(); primary.click() }
        else if (current.onclick || current.getAttribute("role") === "button") { event.preventDefault(); current.click() }
        return
      }
      if (key === "x") {
        const toggle = current.querySelector<HTMLElement>('[data-nav-select], input[type="checkbox"], [role="checkbox"]')
        if (toggle) { event.preventDefault(); toggle.click() }
      }
    }
    const openHelp = () => setHelpOpen(true)
    window.addEventListener("keydown", onKeyDown)
    window.addEventListener(OPEN_SHORTCUTS_EVENT, openHelp)
    return () => {
      window.removeEventListener("keydown", onKeyDown)
      window.removeEventListener(OPEN_SHORTCUTS_EVENT, openHelp)
      clearSequence()
    }
  }, [clearSequence, helpOpen, navigate, quickLook, toggleSidebar])

  return (
    <Dialog open={helpOpen} onOpenChange={setHelpOpen}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
          <DialogDescription>Shortcuts are paused while you type in a field.</DialogDescription>
        </DialogHeader>
        <div className="grid max-h-[65vh] gap-x-8 gap-y-5 overflow-y-auto sm:grid-cols-2">
          {SHORTCUT_GROUPS.map(group => (
            <section key={group.title} aria-labelledby={`shortcuts-${group.title}`}>
              <h3 id={`shortcuts-${group.title}`} className="mb-1.5 text-[11px] font-semibold text-muted-foreground">{group.title}</h3>
              <dl className="grid gap-1">
                {group.items.map(item => (
                  <div key={item.label} className="flex items-center justify-between gap-3 text-sm">
                    <dt>{item.label}</dt>
                    <dd className="flex shrink-0 items-center gap-1">
                      {item.keys.map((k, i) => (
                        <span key={i} className="flex items-center gap-1">
                          {i > 0 && group.title === "Go to" && <span className="text-[11px] text-muted-foreground">then</span>}
                          <Kbd>{k}</Kbd>
                        </span>
                      ))}
                    </dd>
                  </div>
                ))}
              </dl>
            </section>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  )
}
