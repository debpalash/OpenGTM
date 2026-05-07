import { useMemo } from "react"
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

interface HybridMessageProps {
  content: string
  isStreaming?: boolean
}

export function HybridMessage({ content, isStreaming = false }: HybridMessageProps) {
  const isOpenUI = useMemo(() => detectOpenUILang(content), [content])

  if (isOpenUI) {
    return (
      <div className="openui-container" style={{ margin: "-8px 0" }}>
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

  return <MarkdownContent content={content} />
}
