/**
 * Chat composer: auto-growing textarea with a `/` command menu, and (on the
 * empty state) action pills above it. Enter sends, Shift+Enter breaks a line;
 * in the menu ↑/↓ move, Enter/Tab pick and Esc closes.
 */
import { forwardRef, useEffect, useImperativeHandle, useLayoutEffect, useMemo, useRef, useState } from "react"
import { useNavigate } from "react-router-dom"
import { ArrowUp, CornerDownLeft, Slash, Square } from "lucide-react"
import { cn } from "@/lib/utils"
import { PILLS, matchCommands, placeholderRange, type ChatAction, type SlashCommand } from "./commands"

export interface ComposerHandle { focus: () => void; apply: (action: ChatAction) => void }

interface ComposerProps {
  value: string
  onChange: (value: string) => void
  onSend: (text?: string) => void
  onStop: () => void
  isLoading: boolean
  variant: "hero" | "dock"
}

export const Composer = forwardRef<ComposerHandle, ComposerProps>(function Composer(
  { value, onChange, onSend, onStop, isLoading, variant }, ref,
) {
  const navigate = useNavigate()
  const textarea = useRef<HTMLTextAreaElement>(null)
  const [active, setActive] = useState(0)
  const [dismissed, setDismissed] = useState<string | null>(null)
  // Selection to apply once a filled-in template has rendered.
  const pendingSelection = useRef<[number, number] | null>(null)

  const slash = /^\/(\S*)$/.exec(value)
  const matches = useMemo(() => (slash ? matchCommands(slash[1]) : []), [slash?.[1]]) // eslint-disable-line react-hooks/exhaustive-deps
  const menuOpen = !!slash && matches.length > 0 && dismissed !== value
  useEffect(() => { setActive(0) }, [slash?.[1]])

  // Grow with content up to the CSS max-height; place a pending selection.
  useLayoutEffect(() => {
    const el = textarea.current
    if (!el) return
    el.style.height = "auto"
    el.style.height = `${el.scrollHeight}px`
    const range = pendingSelection.current
    if (range) {
      pendingSelection.current = null
      el.focus()
      el.setSelectionRange(range[0], range[1])
    }
  }, [value])

  const apply = (action: ChatAction) => {
    if (action.kind === "go") { onChange(""); navigate(action.to); return }
    if (action.run) { onSend(action.text); return }
    pendingSelection.current = placeholderRange(action.text) ?? [action.text.length, action.text.length]
    onChange(action.text)
  }
  const pick = (command: SlashCommand) => apply(command.action)

  useImperativeHandle(ref, () => ({ focus: () => textarea.current?.focus(), apply }))

  const onKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (menuOpen) {
      if (event.key === "ArrowDown") { event.preventDefault(); setActive(i => (i + 1) % matches.length); return }
      if (event.key === "ArrowUp") { event.preventDefault(); setActive(i => (i - 1 + matches.length) % matches.length); return }
      if (event.key === "Enter" || event.key === "Tab") { event.preventDefault(); pick(matches[active]); return }
      if (event.key === "Escape") { event.preventDefault(); setDismissed(value); return }
    }
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault()
      onSend()
    }
  }

  // Tab to the next [placeholder] while filling a template.
  const onKeyDownCapture = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (menuOpen || event.key !== "Tab" || event.shiftKey) return
    const el = event.currentTarget
    const rest = value.slice(el.selectionEnd)
    const range = placeholderRange(rest)
    if (!range) return
    event.preventDefault()
    el.setSelectionRange(el.selectionEnd + range[0], el.selectionEnd + range[1])
  }

  let lastGroup = ""
  const hero = variant === "hero"

  return (
    <div className={cn("gtm-composer relative w-full", hero ? "max-w-2xl" : "max-w-3xl")}>
      {hero && (
        <div className="gtm-composer-pills mb-3 flex gap-1.5 overflow-x-auto pb-1 sm:flex-wrap sm:justify-center sm:overflow-visible" role="toolbar" aria-label="Quick actions">
          {PILLS.map(pill => (
            <button key={pill.name} type="button" onClick={() => pick(pill)}
              className="gtm-chat-pill inline-flex shrink-0 items-center gap-1.5 rounded-full px-3 py-1.5 text-[13px] font-medium">
              <pill.icon className="size-3.5" aria-hidden="true" />
              {pill.label}
            </button>
          ))}
        </div>
      )}

      {menuOpen && (
        <div id="gtm-slash-menu" role="listbox" aria-label="Commands"
          className={cn("gtm-slash-menu absolute inset-x-0 z-30 max-h-[340px] overflow-y-auto rounded-2xl p-1.5",
            hero ? "top-full mt-2" : "bottom-full mb-2")}>
          {matches.map((command, i) => {
            const header = command.group !== lastGroup ? (lastGroup = command.group) : null
            return (
              <div key={command.name}>
                {header && <div className="px-2.5 pt-2 pb-1 text-[11px] font-medium tracking-wide text-muted-foreground uppercase">{header}</div>}
                <button type="button" role="option" id={`gtm-slash-${command.name}`} aria-selected={i === active}
                  onMouseEnter={() => setActive(i)} onMouseDown={e => e.preventDefault()} onClick={() => pick(command)}
                  className={cn("flex w-full items-center gap-3 rounded-xl px-2.5 py-2 text-left", i === active && "bg-[var(--gtm-chat-hover)]")}>
                  <span className="gtm-slash-icon flex size-8 shrink-0 items-center justify-center rounded-lg">
                    <command.icon className="size-4" aria-hidden="true" />
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="block text-[14px] font-medium text-foreground">
                      <span className="text-muted-foreground">/</span>{command.name}
                      <span className="ml-2 font-normal text-muted-foreground">{command.label}</span>
                    </span>
                    <span className="block truncate text-[12px] text-muted-foreground">{command.hint}</span>
                  </span>
                  {i === active && <CornerDownLeft className="size-3.5 text-muted-foreground" aria-hidden="true" />}
                </button>
              </div>
            )
          })}
        </div>
      )}

      <div className="gtm-composer-box flex flex-col rounded-[26px]">
        <textarea
          ref={textarea}
          rows={1}
          value={value}
          onChange={e => { onChange(e.target.value); setDismissed(null) }}
          onKeyDown={onKeyDown}
          onKeyDownCapture={onKeyDownCapture}
          disabled={isLoading}
          placeholder={hero ? "Ask for a list, a person, a brief… or type / for commands" : "Reply, or type / for commands"}
          aria-label="Message OpenGTM"
          role="combobox"
          aria-expanded={menuOpen}
          aria-controls={menuOpen ? "gtm-slash-menu" : undefined}
          aria-autocomplete="list"
          aria-activedescendant={menuOpen ? `gtm-slash-${matches[active]?.name}` : undefined}
          className={cn("w-full resize-none bg-transparent px-5 text-[15px] leading-6 text-foreground outline-none placeholder:text-muted-foreground/70",
            hero ? "min-h-[76px] max-h-[240px] pt-4" : "min-h-[48px] max-h-[200px] pt-3.5")}
        />
        <div className="flex items-center gap-2 px-3 pb-3">
          <button type="button" onClick={() => { onChange("/"); setDismissed(null); textarea.current?.focus() }}
            className="gtm-composer-tool inline-flex h-8 items-center gap-1.5 rounded-full px-2.5 text-[12px] font-medium" aria-label="Commands">
            <Slash className="size-3.5" aria-hidden="true" /> Commands
          </button>
          <span className="ml-auto hidden text-[11px] text-muted-foreground sm:inline">
            <kbd className="gtm-kbd">Enter</kbd> send · <kbd className="gtm-kbd">Shift</kbd>+<kbd className="gtm-kbd">Enter</kbd> new line
          </span>
          {isLoading ? (
            <button type="button" onClick={onStop} title="Stop generating" aria-label="Stop generating"
              className="flex size-9 items-center justify-center rounded-full bg-foreground text-background transition-transform hover:scale-105 active:scale-95 sm:ml-2">
              <Square className="size-3.5 fill-current" />
            </button>
          ) : (
            <button type="button" onClick={() => onSend()} disabled={!value.trim() || menuOpen} aria-label="Send"
              className="gtm-send flex size-9 items-center justify-center rounded-full transition-transform enabled:hover:scale-105 enabled:active:scale-95 disabled:opacity-35 max-sm:ml-auto sm:ml-2">
              <ArrowUp className="size-4" />
            </button>
          )}
        </div>
      </div>
    </div>
  )
})
