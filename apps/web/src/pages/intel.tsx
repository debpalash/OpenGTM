import { useState } from "react"
import { Search, Loader2, Globe, Mail, Phone, Building2, MapPin, Briefcase, ExternalLink } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"

const API_BASE = ""

interface PersonResult {
  name: string
  title?: string
  company?: string
  location?: string
  linkedin_url?: string
  email?: string
  phone?: string
  website?: string
  bio?: string
  profile_image?: string
  source?: string
  confidence?: number
}

export default function IntelPage() {
  const [query, setQuery] = useState("")
  const [results, setResults] = useState<PersonResult[]>([])
  const [isSearching, setIsSearching] = useState(false)
  const [error, setError] = useState("")

  const handleSearch = async () => {
    if (!query.trim()) return
    setIsSearching(true)
    setError("")
    try {
      const res = await fetch(`${API_BASE}/api/v2/person-intel/search`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query: query.trim(), max_results: 20 }),
      })
      const data = await res.json()
      setResults(data.results || data.profiles || [])
    } catch (err) {
      setError("Search failed. Make sure the API server is running.")
      setResults([])
    } finally {
      setIsSearching(false)
    }
  }

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* Header */}
      <div className="p-4 border-b border-border space-y-2">
        <h2 className="text-sm font-semibold flex items-center gap-2">
          <Briefcase size={16} className="text-primary" />
          Person Intelligence
        </h2>
        <p className="text-[11px] text-muted-foreground">Search for people across LinkedIn, web profiles, and public records</p>
        <div className="flex gap-2">
          <Input
            value={query}
            onChange={e => setQuery(e.target.value)}
            onKeyDown={e => e.key === "Enter" && handleSearch()}
            placeholder="e.g. John Smith CTO Bangalore"
            className="h-9 bg-card"
          />
          <Button onClick={handleSearch} disabled={isSearching} className="h-9 px-4">
            {isSearching ? <Loader2 size={14} className="animate-spin mr-1.5" /> : <Search size={14} className="mr-1.5" />}
            Search
          </Button>
        </div>
        {error && <p className="text-xs text-destructive">{error}</p>}
      </div>

      {/* Results */}
      <ScrollArea className="flex-1">
        <div className="p-4 space-y-3">
          {results.length === 0 && !isSearching && (
            <div className="text-center py-20 text-muted-foreground">
              <Briefcase className="mx-auto mb-3 opacity-20" size={48} />
              <p className="text-sm">Search for people to discover profiles and contact info</p>
              <p className="text-xs mt-1 text-muted-foreground/60">Uses DDG Lite, LinkedIn enrichment, and web OSINT</p>
            </div>
          )}

          {isSearching && (
            <div className="text-center py-20">
              <Loader2 className="mx-auto mb-3 animate-spin text-primary" size={32} />
              <p className="text-sm text-muted-foreground">Searching across sources…</p>
            </div>
          )}

          {results.map((person, i) => (
            <Card key={i} className="p-4 hover:border-primary/40 transition-all">
              <div className="flex gap-4">
                {/* Avatar */}
                <div className="w-14 h-14 rounded-full bg-card border border-border overflow-hidden shrink-0 flex items-center justify-center text-lg font-bold text-primary/30">
                  {person.profile_image ? (
                    <img src={person.profile_image} className="w-full h-full object-cover" />
                  ) : (
                    person.name?.charAt(0)?.toUpperCase() || "?"
                  )}
                </div>

                {/* Info */}
                <div className="flex-1 min-w-0 space-y-1.5">
                  <div className="flex items-center gap-2">
                    <h3 className="text-sm font-semibold truncate">{person.name}</h3>
                    {person.confidence && (
                      <Badge variant="secondary" className="text-[9px]">
                        {Math.round(person.confidence * 100)}% match
                      </Badge>
                    )}
                  </div>

                  {person.title && (
                    <p className="text-xs text-muted-foreground flex items-center gap-1.5">
                      <Briefcase size={11} className="shrink-0" /> {person.title}
                    </p>
                  )}
                  {person.company && (
                    <p className="text-xs text-muted-foreground flex items-center gap-1.5">
                      <Building2 size={11} className="shrink-0" /> {person.company}
                    </p>
                  )}
                  {person.location && (
                    <p className="text-xs text-muted-foreground flex items-center gap-1.5">
                      <MapPin size={11} className="shrink-0" /> {person.location}
                    </p>
                  )}

                  {/* Contact row */}
                  <div className="flex flex-wrap gap-2 mt-2">
                    {person.email && (
                      <a href={`mailto:${person.email}`} className="flex items-center gap-1 text-[10px] text-primary hover:underline">
                        <Mail size={10} /> {person.email}
                      </a>
                    )}
                    {person.phone && (
                      <span className="flex items-center gap-1 text-[10px] text-muted-foreground">
                        <Phone size={10} /> {person.phone}
                      </span>
                    )}
                    {person.linkedin_url && (
                      <a href={person.linkedin_url} target="_blank" rel="noreferrer" className="flex items-center gap-1 text-[10px] text-blue-400 hover:underline">
                        <ExternalLink size={10} /> LinkedIn
                      </a>
                    )}
                    {person.website && (
                      <a href={person.website} target="_blank" rel="noreferrer" className="flex items-center gap-1 text-[10px] text-muted-foreground hover:text-foreground">
                        <Globe size={10} /> Website
                      </a>
                    )}
                  </div>

                  {person.bio && <p className="text-[11px] text-muted-foreground/80 mt-1 line-clamp-2">{person.bio}</p>}
                </div>

                {/* Actions */}
                <div className="flex flex-col gap-1 shrink-0">
                  {person.linkedin_url && (
                    <Button variant="outline" size="icon" className="h-7 w-7" onClick={() => window.open(person.linkedin_url, '_blank')}>
                      <ExternalLink size={12} />
                    </Button>
                  )}
                </div>
              </div>
            </Card>
          ))}
        </div>
      </ScrollArea>
    </div>
  )
}
