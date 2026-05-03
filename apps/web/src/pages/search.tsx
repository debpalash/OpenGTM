import { useState } from "react"
import { toast } from "sonner"
import {
  Search as SearchIcon, Globe, MapPin, BookOpen,
  Loader2, Plus,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Separator } from "@/components/ui/separator"
import { useCollect } from "@/lib/hooks"

const PRESETS = [
  { label: "IT Staffing India", query: "IT staffing companies Bangalore" },
  { label: "HR Consulting Pune", query: "HR consulting agencies Pune" },
  { label: "SaaS Founders", query: "SaaS companies India founders" },
  { label: "Ecommerce Brands", query: "ecommerce brands Mumbai" },
  { label: "Marketing Agencies", query: "digital marketing agencies Delhi" },
  { label: "Recruitment Firms", query: "recruitment firms Hyderabad" },
]

export default function SearchPage() {
  const [query, setQuery] = useState("")
  const collect = useCollect()

  const handleSearch = () => {
    if (!query.trim()) return
    collect.mutate({ query })
    toast.success(`Search started: "${query}"`)
    setQuery("")
  }

  return (
    <div className="p-6 max-w-3xl mx-auto space-y-6">
      <div>
        <h2 className="text-lg font-semibold">Search</h2>
        <p className="text-sm text-muted-foreground">
          Find companies across multiple sources. Results are processed by AI agents.
        </p>
      </div>

      {/* Search Bar */}
      <div className="flex items-center gap-2">
        <div className="relative flex-1">
          <SearchIcon className="absolute left-3 top-3 size-4 text-muted-foreground" />
          <Input
            placeholder="e.g. IT staffing companies in Bangalore..."
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && handleSearch()}
            className="pl-9 h-10"
          />
        </div>
        <Button onClick={handleSearch} disabled={collect.isPending || !query.trim()}>
          {collect.isPending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
          Search
        </Button>
      </div>

      <Separator />

      {/* Source Tabs */}
      <Tabs defaultValue="all">
        <TabsList>
          <TabsTrigger value="all">All Sources</TabsTrigger>
          <TabsTrigger value="web">
            <Globe className="size-3 mr-1" /> Web
          </TabsTrigger>
          <TabsTrigger value="maps">
            <MapPin className="size-3 mr-1" /> Maps
          </TabsTrigger>
          <TabsTrigger value="directories">
            <BookOpen className="size-3 mr-1" /> Directories
          </TabsTrigger>
        </TabsList>

        <TabsContent value="all" className="mt-4">
          <div className="text-sm text-muted-foreground">
            Searches all configured sources simultaneously.
          </div>
        </TabsContent>
        <TabsContent value="web" className="mt-4">
          <div className="text-sm text-muted-foreground">
            DuckDuckGo web search — finds company websites directly.
          </div>
        </TabsContent>
        <TabsContent value="maps" className="mt-4">
          <div className="text-sm text-muted-foreground">
            Google Maps — local business listings with reviews and location data.
          </div>
        </TabsContent>
        <TabsContent value="directories" className="mt-4">
          <div className="text-sm text-muted-foreground">
            Business directories — JustDial, IndiaMart, Yellow Pages, etc.
          </div>
        </TabsContent>
      </Tabs>

      <Separator />

      {/* Presets */}
      <div>
        <h3 className="text-sm font-medium mb-3">Quick Searches</h3>
        <div className="grid gap-2 md:grid-cols-3">
          {PRESETS.map((preset) => (
            <button
              key={preset.query}
              className="text-left p-3 rounded-lg border hover:bg-muted/50 transition-colors"
              onClick={() => {
                collect.mutate({ query: preset.query })
                toast.success(`Started: "${preset.query}"`)
              }}
            >
              <div className="text-sm font-medium">{preset.label}</div>
              <div className="text-xs text-muted-foreground mt-0.5 truncate">{preset.query}</div>
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}
