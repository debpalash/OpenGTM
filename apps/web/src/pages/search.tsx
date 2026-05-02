import { useState, useEffect } from "react"
import { Search, Grid3X3, List, AlignJustify, Download, Eye, Bookmark, Loader2, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Card } from "@/components/ui/card"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

const API_BASE = ""

interface SearchResult {
  id?: string
  title: string
  url: string
  source: string
  thumbnail?: string
  description?: string
  snippet?: string
  author?: string
  year?: string
  file_type?: string
  file_size?: string
  page_count?: number
}

interface Source {
  name: string
  display_name: string
  available: boolean
  count?: number
}

const SOURCE_COLORS: Record<string, string> = {
  scribd: "bg-orange-500/15 text-orange-400 border-orange-500/30",
  arxiv: "bg-red-500/15 text-red-400 border-red-500/30",
  gutenberg: "bg-emerald-500/15 text-emerald-400 border-emerald-500/30",
  libgen: "bg-blue-500/15 text-blue-400 border-blue-500/30",
  pdfdrive: "bg-purple-500/15 text-purple-400 border-purple-500/30",
  pubmed: "bg-cyan-500/15 text-cyan-400 border-cyan-500/30",
  doaj: "bg-yellow-500/15 text-yellow-400 border-yellow-500/30",
  openlibrary: "bg-pink-500/15 text-pink-400 border-pink-500/30",
}

export default function SearchPage() {
  const [query, setQuery] = useState("")
  const [debouncedQuery, setDebouncedQuery] = useState("")
  const [results, setResults] = useState<SearchResult[]>([])
  const [isSearching, setIsSearching] = useState(false)
  const [sources, setSources] = useState<Source[]>([])
  const [selectedSources, setSelectedSources] = useState<string[]>([])
  const [viewMode, setViewMode] = useState<"grid" | "list" | "compact">("grid")
  const [sortBy, setSortBy] = useState("relevance")
  const [selectedResults, setSelectedResults] = useState<Set<number>>(new Set())
  const [bookmarks, setBookmarks] = useState<Set<string>>(() => {
    try {
      return new Set(JSON.parse(localStorage.getItem("doc_bookmarks") || "[]"))
    } catch { return new Set() }
  })

  // Load sources
  useEffect(() => {
    fetch(`${API_BASE}/api/sources`)
      .then(r => r.json())
      .then(data => {
        const srcs = data.sources || []
        setSources(srcs)
        setSelectedSources(srcs.filter((s: Source) => s.available).map((s: Source) => s.name))
      })
      .catch(() => setSources([]))
  }, [])

  // Debounce
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query), 400)
    return () => clearTimeout(t)
  }, [query])

  // Search
  useEffect(() => {
    if (!debouncedQuery) { setResults([]); return }
    setIsSearching(true)
    const params = new URLSearchParams({
      q: debouncedQuery,
      sources: selectedSources.join(","),
      limit: "30",
      sort: sortBy,
    })
    fetch(`${API_BASE}/api/v2/search/unified?${params}`)
      .then(r => r.json())
      .then(data => setResults(data.results || []))
      .catch(() => setResults([]))
      .finally(() => setIsSearching(false))
  }, [debouncedQuery, selectedSources, sortBy])

  // Persist bookmarks
  useEffect(() => {
    localStorage.setItem("doc_bookmarks", JSON.stringify([...bookmarks]))
  }, [bookmarks])

  const toggleBookmark = (url: string) => {
    setBookmarks(prev => {
      const next = new Set(prev)
      next.has(url) ? next.delete(url) : next.add(url)
      return next
    })
  }

  const toggleSelect = (idx: number) => {
    setSelectedResults(prev => {
      const next = new Set(prev)
      next.has(idx) ? next.delete(idx) : next.add(idx)
      return next
    })
  }

  const queueDownload = (url: string) => {
    fetch(`${API_BASE}/api/v2/tasks`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url }),
    }).catch(() => {})
  }

  const toggleSource = (name: string) => {
    setSelectedSources(prev =>
      prev.includes(name) ? prev.filter(s => s !== name) : [...prev, name]
    )
  }

  const getSourceColor = (source: string) =>
    SOURCE_COLORS[source] || "bg-zinc-500/15 text-zinc-400 border-zinc-500/30"

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* ─── Search Bar ─── */}
      <div className="p-4 border-b border-border space-y-3">
        <div className="relative">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" size={16} />
          <Input
            value={query}
            onChange={e => setQuery(e.target.value)}
            placeholder="Search documents, books, research papers…"
            className="pl-10 pr-24 h-10 bg-card border-border"
          />
          <div className="absolute right-2 top-1/2 -translate-y-1/2 flex items-center gap-1">
            {isSearching && <Loader2 className="animate-spin text-primary" size={14} />}
            <Button variant="ghost" size="icon" className={`h-7 w-7 ${viewMode === "grid" ? "bg-primary/20 text-primary" : ""}`} onClick={() => setViewMode("grid")}><Grid3X3 size={14} /></Button>
            <Button variant="ghost" size="icon" className={`h-7 w-7 ${viewMode === "list" ? "bg-primary/20 text-primary" : ""}`} onClick={() => setViewMode("list")}><List size={14} /></Button>
            <Button variant="ghost" size="icon" className={`h-7 w-7 ${viewMode === "compact" ? "bg-primary/20 text-primary" : ""}`} onClick={() => setViewMode("compact")}><AlignJustify size={14} /></Button>
          </div>
        </div>

        {/* Source chips + sort */}
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[10px] uppercase font-bold text-muted-foreground tracking-wider">Sources</span>
          <div className="flex gap-1.5 flex-wrap">
            {sources.map(src => (
              <button
                key={src.name}
                onClick={() => toggleSource(src.name)}
                disabled={!src.available}
                className={`px-2 py-0.5 rounded-full text-[10px] font-medium border transition-all ${
                  selectedSources.includes(src.name)
                    ? getSourceColor(src.name)
                    : src.available
                      ? "bg-card border-border text-muted-foreground hover:border-primary/40"
                      : "bg-card/50 border-border/50 text-muted-foreground/30 cursor-not-allowed"
                }`}
              >
                {src.display_name}
              </button>
            ))}
          </div>
          <div className="ml-auto flex items-center gap-2">
            <Select value={sortBy} onValueChange={(v) => v && setSortBy(v)}>
              <SelectTrigger className="h-7 w-28 text-[11px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="relevance">Relevance</SelectItem>
                <SelectItem value="date">Newest</SelectItem>
                <SelectItem value="title">A-Z</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>
      </div>

      {/* ─── Bulk Action Bar ─── */}
      {selectedResults.size > 0 && (
        <div className="px-4 py-2 bg-primary/10 border-b border-primary/20 flex items-center gap-3">
          <span className="text-xs font-bold text-primary">{selectedResults.size} selected</span>
          <Button size="sm" variant="default" className="h-7 text-xs" onClick={() => {
            selectedResults.forEach(idx => { if (results[idx]) queueDownload(results[idx].url) })
            setSelectedResults(new Set())
          }}>
            <Download size={12} className="mr-1.5" /> Queue All
          </Button>
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => setSelectedResults(new Set())}>
            <X size={12} className="mr-1" /> Clear
          </Button>
        </div>
      )}

      {/* ─── Results ─── */}
      <ScrollArea className="flex-1">
        <div className="p-4">
          {results.length === 0 && !isSearching && (
            <div className="text-center py-20 text-muted-foreground">
              <Search className="mx-auto mb-3 opacity-20" size={48} />
              <p className="text-sm">{debouncedQuery ? "No results found" : "Search across 15+ document sources"}</p>
              <p className="text-xs mt-1 text-muted-foreground/60">Scribd · LibGen · arXiv · Gutenberg · PubMed · DOAJ · OpenLibrary · PDFDrive · and more</p>
            </div>
          )}

          {viewMode === "grid" && (
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-3">
              {results.map((r, i) => (
                <Card key={i} className={`group relative overflow-hidden transition-all hover:border-primary/40 ${selectedResults.has(i) ? "ring-2 ring-primary border-primary" : ""}`}>
                  <input type="checkbox" checked={selectedResults.has(i)} onChange={() => toggleSelect(i)} className="absolute top-2 left-2 z-10 cursor-pointer" />
                  <button onClick={() => toggleBookmark(r.url)} className={`absolute top-2 right-2 z-10 p-1 rounded-full transition-colors ${bookmarks.has(r.url) ? "text-yellow-400 bg-yellow-400/10" : "text-muted-foreground/30 hover:text-muted-foreground"}`}>
                    <Bookmark size={14} fill={bookmarks.has(r.url) ? "currentColor" : "none"} />
                  </button>
                  <div className="aspect-[3/4] bg-card relative overflow-hidden">
                    {r.thumbnail ? (
                      <img src={r.thumbnail} alt={r.title} className="w-full h-full object-cover group-hover:scale-105 transition-transform duration-500" />
                    ) : (
                      <div className="w-full h-full flex flex-col items-center justify-center text-muted-foreground/20">
                        <Search size={24} />
                        <span className="text-[9px] uppercase tracking-widest mt-2 font-bold">{r.source}</span>
                      </div>
                    )}
                    <div className="absolute bottom-0 inset-x-0 bg-gradient-to-t from-background to-transparent p-2 flex justify-between opacity-0 group-hover:opacity-100 transition-opacity">
                      <Badge variant="secondary" className="text-[9px]">{r.file_type?.toUpperCase() || "DOC"}</Badge>
                      {r.year && <span className="text-[10px] text-muted-foreground font-mono">{r.year}</span>}
                    </div>
                  </div>
                  <div className="p-3 space-y-1.5">
                    <h3 className="text-xs font-medium line-clamp-2 group-hover:text-primary transition-colors">{r.title}</h3>
                    {r.author && <p className="text-[10px] text-muted-foreground">by {r.author}</p>}
                    <div className="flex items-center justify-between">
                      <Badge variant="outline" className={`text-[9px] ${getSourceColor(r.source)}`}>{r.source}</Badge>
                      <div className="flex gap-1">
                        <Button variant="ghost" size="icon" className="h-6 w-6" onClick={() => window.open(r.url, "_blank")}><Eye size={12} /></Button>
                        <Button variant="default" size="icon" className="h-6 w-6" onClick={() => queueDownload(r.url)}><Download size={12} /></Button>
                      </div>
                    </div>
                  </div>
                </Card>
              ))}
            </div>
          )}

          {viewMode === "list" && (
            <div className="space-y-2">
              {results.map((r, i) => (
                <Card key={i} className={`flex items-center gap-3 p-3 group hover:border-primary/40 transition-all ${selectedResults.has(i) ? "ring-2 ring-primary" : ""}`}>
                  <input type="checkbox" checked={selectedResults.has(i)} onChange={() => toggleSelect(i)} className="cursor-pointer shrink-0" />
                  <div className="w-12 h-16 rounded bg-card border border-border overflow-hidden shrink-0">
                    {r.thumbnail ? <img src={r.thumbnail} className="w-full h-full object-cover" /> : <div className="w-full h-full flex items-center justify-center text-muted-foreground/20"><Search size={14} /></div>}
                  </div>
                  <div className="flex-1 min-w-0">
                    <h3 className="text-xs font-medium truncate group-hover:text-primary transition-colors">{r.title}</h3>
                    <div className="flex items-center gap-2 mt-0.5 text-[10px] text-muted-foreground">
                      {r.author && <span>by {r.author}</span>}
                      {r.year && <span>· {r.year}</span>}
                      {r.file_type && <span>· {r.file_type.toUpperCase()}</span>}
                    </div>
                  </div>
                  <Badge variant="outline" className={`text-[9px] shrink-0 ${getSourceColor(r.source)}`}>{r.source}</Badge>
                  <div className="flex gap-1 shrink-0">
                    <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => window.open(r.url, "_blank")}><Eye size={14} /></Button>
                    <Button variant="default" size="icon" className="h-7 w-7" onClick={() => queueDownload(r.url)}><Download size={14} /></Button>
                    <Button variant="ghost" size="icon" className={`h-7 w-7 ${bookmarks.has(r.url) ? "text-yellow-400" : ""}`} onClick={() => toggleBookmark(r.url)}>
                      <Bookmark size={14} fill={bookmarks.has(r.url) ? "currentColor" : "none"} />
                    </Button>
                  </div>
                </Card>
              ))}
            </div>
          )}

          {viewMode === "compact" && (
            <div className="border border-border rounded-lg overflow-hidden">
              <table className="w-full text-xs">
                <thead className="bg-card border-b border-border">
                  <tr>
                    <th className="p-2 w-8"><input type="checkbox" onChange={e => e.target.checked ? setSelectedResults(new Set(results.map((_, i) => i))) : setSelectedResults(new Set())} /></th>
                    <th className="p-2 text-left text-muted-foreground font-medium">Title</th>
                    <th className="p-2 text-left text-muted-foreground font-medium">Source</th>
                    <th className="p-2 text-left text-muted-foreground font-medium">Year</th>
                    <th className="p-2 text-left text-muted-foreground font-medium">Type</th>
                    <th className="p-2 text-right text-muted-foreground font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {results.map((r, i) => (
                    <tr key={i} className={`border-b border-border/50 hover:bg-card/80 ${selectedResults.has(i) ? "bg-primary/5" : ""}`}>
                      <td className="p-2"><input type="checkbox" checked={selectedResults.has(i)} onChange={() => toggleSelect(i)} /></td>
                      <td className="p-2 max-w-xs truncate">{r.title}</td>
                      <td className="p-2"><Badge variant="outline" className={`text-[9px] ${getSourceColor(r.source)}`}>{r.source}</Badge></td>
                      <td className="p-2 text-muted-foreground">{r.year || "—"}</td>
                      <td className="p-2 text-muted-foreground uppercase">{r.file_type || "—"}</td>
                      <td className="p-2 text-right">
                        <div className="flex gap-1 justify-end">
                          <Button variant="ghost" size="icon" className="h-6 w-6" onClick={() => window.open(r.url, "_blank")}><Eye size={12} /></Button>
                          <Button variant="default" size="icon" className="h-6 w-6" onClick={() => queueDownload(r.url)}><Download size={12} /></Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </ScrollArea>
    </div>
  )
}
