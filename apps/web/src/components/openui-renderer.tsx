import { useMemo, useState, useEffect, useRef } from "react"
import { Renderer } from "@openuidev/react-lang"
import { yupchLibrary } from "@/lib/openui-library"
import { MarkdownContent } from "@/components/markdown-content"

/**
 * Detects if a response string is likely OpenUI Lang syntax.
 * Looks for assignments like `identifier = Component(...)`
 */
function detectOpenUILang(text: string): boolean {
  if (!text) return false

  // Basic heuristic: contains root assignment
  if (text.includes("root = Root") || text.includes("root = ")) {
    return true
  }

  // Advanced heuristic: looks for multiple lines of identifier = ComponentName(
  const lines = text.split("\n")
  let assignmentCount = 0
  for (const line of lines) {
    const trimmed = line.trim()
    if (trimmed.match(/^[a-zA-Z0-9_]+\s*=\s*[A-Z][a-zA-Z0-9_]*\(/)) {
      assignmentCount++
    }
  }

  return assignmentCount >= 1
}

/**
 * When we fall back to plain text, strip the OpenUI Lang block (which the
 * model often appends after a prose answer) so the user sees the clean
 * natural-language answer instead of raw `root = Root([...])` code.
 */
function stripDslBlock(text: string): string {
  const inline = text.indexOf("root = Root")
  const lineMatch = text.search(/(^|\n)\s*[a-zA-Z0-9_]+\s*=\s*[A-Z][a-zA-Z0-9_]*\(/)
  let cut = -1
  if (inline >= 0) cut = inline
  if (lineMatch >= 0) cut = cut === -1 ? lineMatch : Math.min(cut, lineMatch)
  if (cut < 0) return text
  return text.slice(0, cut).trim()
}

/**
 * Pull the human-readable sentences out of a pure-DSL response — the model
 * embeds the actual answer in component strings (InfoCallout content, etc.).
 * We surface those sentences so the user reads a clean answer instead of raw
 * `InfoCallout(...)` code.
 */
function readableFromDsl(text: string): string {
  return [...text.matchAll(/"([^"]{20,})"/g)]
    .map(m => m[1].trim())
    .filter(s => /[.!?]/.test(s) || s.split(/\s+/).length >= 4)
    .join("\n\n")
}

/** Best-effort clean text for the fallback path. */
function fallbackText(text: string): string {
  const prose = stripDslBlock(text)
  if (prose.length > 0) return prose          // had a prose answer before the DSL
  const extracted = readableFromDsl(text)     // pure DSL → pull embedded sentences
  return extracted.length > 0 ? extracted : text
}

interface HybridMessageProps {
  content: string
  isStreaming?: boolean
}

/**
 * Renders an assistant message. If it's OpenUI Lang, render it with the live
 * Renderer — but the renderer can silently produce nothing for a truncated or
 * otherwise unrenderable response. To guarantee the user never sees a blank
 * bubble, we watch the rendered output and fall back to plain text/markdown if
 * the OpenUI container comes up empty.
 */
export function HybridMessage({ content, isStreaming = false }: HybridMessageProps) {
  const isOpenUI = useMemo(() => detectOpenUILang(content), [content])
  const [fellBack, setFellBack] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)

  // Reset whenever the message changes.
  useEffect(() => { setFellBack(false) }, [content])

  // Once streaming is done, if the OpenUI container rendered no visible
  // content, fall back to plain text so the message is never blank.
  useEffect(() => {
    if (!isOpenUI || isStreaming || fellBack) return
    const id = setTimeout(() => {
      const el = containerRef.current
      const text = el?.textContent?.trim() ?? ""
      if (text.length === 0) setFellBack(true)
    }, 250)
    return () => clearTimeout(id)
  }, [content, isOpenUI, isStreaming, fellBack])

  if (isOpenUI && !fellBack) {
    return (
      <div className="openui-container" style={{ margin: "-8px 0" }} ref={containerRef}>
        <Renderer
          response={content}
          library={yupchLibrary}
          isStreaming={isStreaming}
          onAction={(event) => {
            console.log("OpenUI Action triggered:", event)
            // Can be expanded to handle custom actions like enrichment later
          }}
        />
      </div>
    )
  }

  // Fallback: the OpenUI block didn't render — show the clean prose answer
  // (or the readable sentences extracted from the DSL) instead of raw code.
  return <MarkdownContent content={isOpenUI ? fallbackText(content) : content} />
}
