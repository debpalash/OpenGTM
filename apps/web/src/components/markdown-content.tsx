import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"

/**
 * Shared markdown renderer for chat messages, AI research content,
 * lead descriptions, and any other markdown-formatted text.
 *
 * Supports GFM tables, code blocks, headings, lists, blockquotes, links.
 */
export function MarkdownContent({ content, className = "" }: { content: string; className?: string }) {
  return (
    <div className={className}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          table: ({ children }) => (
            <div className="overflow-x-auto my-3 rounded-lg border border-border/40 shadow-sm">
              <table className="w-full text-sm">{children}</table>
            </div>
          ),
          thead: ({ children }) => (
            <thead className="bg-muted/60 text-xs uppercase tracking-wider">{children}</thead>
          ),
          th: ({ children }) => (
            <th className="px-3 py-2.5 text-left font-medium text-muted-foreground whitespace-nowrap">{children}</th>
          ),
          td: ({ children }) => (
            <td className="px-3 py-2 border-t border-border/20">{children}</td>
          ),
          strong: ({ children }) => (
            <strong className="font-semibold text-foreground">{children}</strong>
          ),
          a: ({ href, children }) => (
            <a href={href} target="_blank" rel="noopener noreferrer"
              className="text-primary underline underline-offset-2 decoration-primary/30 hover:decoration-primary/60 transition-colors">
              {children}
            </a>
          ),
          ul: ({ children }) => <ul className="list-disc list-inside space-y-0.5 my-1.5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal list-inside space-y-0.5 my-1.5">{children}</ol>,
          li: ({ children }) => <li className="text-sm leading-relaxed">{children}</li>,
          code: ({ children, className: cn }) => {
            const isInline = !cn
            return isInline ? (
              <code className="bg-muted/80 px-1.5 py-0.5 rounded text-[13px] font-mono text-foreground/90">{children}</code>
            ) : (
              <pre className="bg-muted/50 rounded-xl p-4 overflow-x-auto my-3 border border-border/20">
                <code className="text-[13px] font-mono">{children}</code>
              </pre>
            )
          },
          h1: ({ children }) => <h1 className="text-lg font-bold mt-4 mb-1.5">{children}</h1>,
          h2: ({ children }) => <h2 className="text-base font-semibold mt-3 mb-1">{children}</h2>,
          h3: ({ children }) => <h3 className="text-sm font-semibold mt-2.5 mb-0.5">{children}</h3>,
          p: ({ children }) => <p className="mb-2 last:mb-0 leading-relaxed">{children}</p>,
          blockquote: ({ children }) => (
            <blockquote className="border-l-2 border-primary/30 pl-3 my-2.5 text-muted-foreground italic">{children}</blockquote>
          ),
          hr: () => <hr className="my-3 border-border/30" />,
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
}
