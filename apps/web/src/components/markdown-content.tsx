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
            <div className="overflow-x-auto my-4 rounded-xl border border-border/40 bg-card/60 shadow-md backdrop-blur-sm">
              <table className="w-full text-sm border-collapse">{children}</table>
            </div>
          ),
          thead: ({ children }) => (
            <thead className="border-b border-border/40" style={{ background: "color-mix(in srgb, var(--muted) 50%, transparent)" }}>{children}</thead>
          ),
          th: ({ children }) => (
            <th className="px-4 py-2.5 text-left text-[11px] font-semibold uppercase tracking-widest text-muted-foreground">{children}</th>
          ),
          tr: ({ children }) => (
            <tr className="border-b border-border/10 last:border-0 transition-colors" style={{ cursor: "default" }}
              onMouseEnter={(e) => (e.currentTarget.style.background = "color-mix(in srgb, var(--muted) 30%, transparent)")}
              onMouseLeave={(e) => (e.currentTarget.style.background = "")}
            >{children}</tr>
          ),
          td: ({ children }) => (
            <td className="px-4 py-3 text-[13px] leading-relaxed align-top">{children}</td>
          ),
          strong: ({ children }) => (
            <strong className="font-semibold text-foreground">{children}</strong>
          ),
          em: ({ children }) => (
            <em className="text-muted-foreground">{children}</em>
          ),
          a: ({ href, children }) => (
            <a href={href} target="_blank" rel="noopener noreferrer"
              className="text-primary underline underline-offset-2 decoration-primary/30 hover:decoration-primary transition-colors">
              {children}
            </a>
          ),
          ul: ({ children }) => <ul className="list-disc list-outside pl-5 space-y-1 my-2">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal list-outside pl-5 space-y-1 my-2">{children}</ol>,
          li: ({ children }) => <li className="text-sm leading-relaxed">{children}</li>,
          code: ({ children, className: cn }) => {
            const isInline = !cn
            return isInline ? (
              <code style={{
                background: "color-mix(in srgb, var(--primary) 8%, transparent)",
                color: "color-mix(in srgb, var(--primary) 85%, transparent)",
                padding: "2px 7px",
                borderRadius: "6px",
                fontSize: "12px",
                fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
                border: "1px solid color-mix(in srgb, var(--primary) 12%, transparent)",
                whiteSpace: "nowrap",
              }}>{children}</code>
            ) : (
              <pre style={{
                background: "color-mix(in srgb, var(--muted) 40%, transparent)",
                borderRadius: "12px",
                padding: "16px",
                overflowX: "auto",
                margin: "12px 0",
                border: "1px solid color-mix(in srgb, var(--border) 20%, transparent)",
              }}>
                <code style={{
                  fontSize: "12.5px",
                  fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
                  lineHeight: "1.6",
                  color: "color-mix(in srgb, var(--foreground) 85%, transparent)",
                }}>{children}</code>
              </pre>
            )
          },
          h1: ({ children }) => <h1 className="text-lg font-bold mt-5 mb-2 tracking-tight">{children}</h1>,
          h2: ({ children }) => <h2 className="text-[15px] font-semibold mt-4 mb-1.5 tracking-tight">{children}</h2>,
          h3: ({ children }) => <h3 className="text-sm font-semibold mt-3 mb-1">{children}</h3>,
          p: ({ children }) => <p className="mb-2.5 last:mb-0 leading-relaxed">{children}</p>,
          blockquote: ({ children }) => (
            <blockquote style={{
              borderLeft: "3px solid color-mix(in srgb, var(--primary) 30%, transparent)",
              paddingLeft: "14px",
              margin: "12px 0",
              background: "color-mix(in srgb, var(--primary) 3%, transparent)",
              borderRadius: "0 8px 8px 0",
              padding: "8px 14px",
            }} className="text-muted-foreground">
              {children}
            </blockquote>
          ),
          hr: () => <hr className="my-4 border-border/30" />,
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
}
