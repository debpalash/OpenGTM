import { useState } from "react"
import { Globe, Loader2, Copy, ExternalLink, Mail, FileText } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"

const API_BASE = ""

interface ScrapeResult {
  url: string
  title?: string
  text?: string
  markdown?: string
  emails?: string[]
  links?: string[]
  screenshots?: string[]
  status: string
  error?: string
}

export default function ScraperPage() {
  const [url, setUrl] = useState("")
  const [result, setResult] = useState<ScrapeResult | null>(null)
  const [isLoading, setIsLoading] = useState(false)
  const [viewTab, setViewTab] = useState<"text" | "markdown" | "emails" | "links">("text")

  const handleScrape = async () => {
    if (!url.trim()) return
    setIsLoading(true)
    setResult(null)
    try {
      const res = await fetch(`${API_BASE}/api/v2/scraper/scrape`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ url: url.trim(), extract_emails: true, extract_links: true }),
      })
      const data = await res.json()
      setResult(data)
    } catch {
      setResult({ url, status: "error", error: "Failed to scrape URL" })
    } finally {
      setIsLoading(false)
    }
  }

  const copyText = (text: string) => {
    navigator.clipboard.writeText(text)
  }

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* Header */}
      <div className="p-4 border-b border-border space-y-2">
        <h2 className="text-sm font-semibold flex items-center gap-2">
          <Globe size={16} className="text-primary" />
          Web Scraper
        </h2>
        <p className="text-[11px] text-muted-foreground">Extract text, emails, and links from any URL. Uses httpx fast path with Playwright fallback.</p>
        <div className="flex gap-2">
          <Input
            value={url}
            onChange={e => setUrl(e.target.value)}
            onKeyDown={e => e.key === "Enter" && handleScrape()}
            placeholder="https://example.com"
            className="h-9 bg-card font-mono text-xs"
          />
          <Button onClick={handleScrape} disabled={isLoading} className="h-9 px-4">
            {isLoading ? <Loader2 size={14} className="animate-spin mr-1.5" /> : <Globe size={14} className="mr-1.5" />}
            Scrape
          </Button>
        </div>
      </div>

      {/* Results */}
      <ScrollArea className="flex-1">
        <div className="p-4">
          {!result && !isLoading && (
            <div className="text-center py-20 text-muted-foreground">
              <Globe className="mx-auto mb-3 opacity-20" size={48} />
              <p className="text-sm">Enter a URL to extract content</p>
              <p className="text-xs mt-1 text-muted-foreground/60">Supports JavaScript-rendered pages via Playwright</p>
            </div>
          )}

          {isLoading && (
            <div className="text-center py-20">
              <Loader2 className="mx-auto mb-3 animate-spin text-primary" size={32} />
              <p className="text-sm text-muted-foreground">Scraping {url}…</p>
            </div>
          )}

          {result && (
            <div className="space-y-4">
              {/* Header card */}
              <Card className="p-3">
                <div className="flex items-center justify-between">
                  <div>
                    <h3 className="text-xs font-semibold">{result.title || result.url}</h3>
                    <a href={result.url} target="_blank" rel="noreferrer" className="text-[10px] text-primary hover:underline flex items-center gap-1 mt-0.5">
                      <ExternalLink size={9} /> {result.url}
                    </a>
                  </div>
                  <Badge variant={result.status === "error" ? "destructive" : "secondary"} className="text-[9px]">
                    {result.status}
                  </Badge>
                </div>
                {result.error && <p className="text-xs text-destructive mt-2">{result.error}</p>}

                {/* Stats row */}
                <div className="flex gap-4 mt-3 text-[10px] text-muted-foreground">
                  {result.text && <span><FileText size={10} className="inline mr-1" />{result.text.length.toLocaleString()} chars</span>}
                  {result.emails && <span><Mail size={10} className="inline mr-1" />{result.emails.length} emails</span>}
                  {result.links && <span><Globe size={10} className="inline mr-1" />{result.links.length} links</span>}
                </div>
              </Card>

              {/* Tab bar */}
              <div className="flex gap-1 border-b border-border">
                {(["text", "markdown", "emails", "links"] as const).map(tab => (
                  <button
                    key={tab}
                    onClick={() => setViewTab(tab)}
                    className={`px-3 py-1.5 text-xs font-medium border-b-2 transition-colors capitalize ${
                      viewTab === tab
                        ? "border-primary text-primary"
                        : "border-transparent text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {tab}
                    {tab === "emails" && result.emails && ` (${result.emails.length})`}
                    {tab === "links" && result.links && ` (${result.links.length})`}
                  </button>
                ))}
              </div>

              {/* Content */}
              {viewTab === "text" && result.text && (
                <Card className="p-4 relative">
                  <Button variant="ghost" size="icon" className="absolute top-2 right-2 h-6 w-6" onClick={() => copyText(result.text || "")}>
                    <Copy size={12} />
                  </Button>
                  <pre className="text-xs text-muted-foreground whitespace-pre-wrap font-mono leading-relaxed max-h-[600px] overflow-auto">
                    {result.text}
                  </pre>
                </Card>
              )}

              {viewTab === "markdown" && result.markdown && (
                <Card className="p-4 relative">
                  <Button variant="ghost" size="icon" className="absolute top-2 right-2 h-6 w-6" onClick={() => copyText(result.markdown || "")}>
                    <Copy size={12} />
                  </Button>
                  <pre className="text-xs text-muted-foreground whitespace-pre-wrap font-mono leading-relaxed max-h-[600px] overflow-auto">
                    {result.markdown}
                  </pre>
                </Card>
              )}

              {viewTab === "emails" && result.emails && (
                <div className="space-y-1">
                  {result.emails.map((email, i) => (
                    <Card key={i} className="px-3 py-2 flex items-center justify-between">
                      <a href={`mailto:${email}`} className="text-xs text-primary hover:underline flex items-center gap-1.5">
                        <Mail size={11} /> {email}
                      </a>
                      <Button variant="ghost" size="icon" className="h-6 w-6" onClick={() => copyText(email)}>
                        <Copy size={11} />
                      </Button>
                    </Card>
                  ))}
                  {result.emails.length === 0 && <p className="text-xs text-muted-foreground text-center py-8">No emails found</p>}
                </div>
              )}

              {viewTab === "links" && result.links && (
                <div className="space-y-1 max-h-[500px] overflow-auto">
                  {result.links.slice(0, 100).map((link, i) => (
                    <Card key={i} className="px-3 py-2">
                      <a href={link} target="_blank" rel="noreferrer" className="text-[11px] text-primary hover:underline truncate block font-mono">
                        {link}
                      </a>
                    </Card>
                  ))}
                  {result.links.length === 0 && <p className="text-xs text-muted-foreground text-center py-8">No links found</p>}
                  {result.links.length > 100 && <p className="text-[10px] text-muted-foreground text-center py-2">+ {result.links.length - 100} more</p>}
                </div>
              )}
            </div>
          )}
        </div>
      </ScrollArea>
    </div>
  )
}
