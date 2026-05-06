import { useState, useEffect } from "react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Switch } from "@/components/ui/switch"
import {
  Loader2, Search, MapPin, BookOpen, Briefcase, BarChart3,
  Users, Rocket, Globe, Database, Zap,
} from "lucide-react"
import { toast } from "sonner"

const API_BASE = import.meta.env.VITE_API_URL || ""

// Map icon name strings from the API to Lucide components
const ICON_MAP: Record<string, React.ComponentType<{ className?: string }>> = {
  "search": Search,
  "map-pin": MapPin,
  "book-open": BookOpen,
  "briefcase": Briefcase,
  "bar-chart-3": BarChart3,
  "users": Users,
  "rocket": Rocket,
  "globe": Globe,
  "database": Database,
  "zap": Zap,
}

interface Source {
  id: string
  name: string
  icon: string
  description: string
  enabled: boolean
  strategy: string
}

export default function SourcesPage() {
  const [sources, setSources] = useState<Source[]>([])
  const [loading, setLoading] = useState(true)
  const [toggling, setToggling] = useState<string | null>(null)

  useEffect(() => {
    fetch(`${API_BASE}/api/settings/sources`)
      .then(r => r.json())
      .then(data => {
        setSources(data)
        setLoading(false)
      })
      .catch(() => setLoading(false))
  }, [])

  const handleToggle = async (sourceId: string, enabled: boolean) => {
    setToggling(sourceId)
    try {
      const res = await fetch(`${API_BASE}/api/settings/sources/${sourceId}?enabled=${enabled}`, {
        method: "PUT",
      })
      if (res.ok) {
        setSources(prev => prev.map(s => s.id === sourceId ? { ...s, enabled } : s))
        toast.success(`${sourceId} ${enabled ? "enabled" : "disabled"}`)
      }
    } catch {
      toast.error("Failed to toggle source")
    } finally {
      setToggling(null)
    }
  }

  if (loading) {
    return (
      <div className="flex justify-center p-12">
        <Loader2 className="size-6 animate-spin text-muted-foreground" />
      </div>
    )
  }

  const activeCount = sources.filter(s => s.enabled).length

  return (
    <div className="p-6 space-y-6">
      <div>
        <h2 className="text-lg font-semibold">Data Sources</h2>
        <p className="text-sm text-muted-foreground">
          Configure which data sources agents use for lead discovery and enrichment.
          <span className="ml-2 text-foreground font-medium">{activeCount}/{sources.length} active</span>
        </p>
      </div>

      <Separator />

      <div className="grid gap-4 md:grid-cols-2">
        {sources.map((source) => {
          const IconComponent = ICON_MAP[source.icon] || Database
          return (
            <Card
              key={source.id}
              className={`transition-all duration-200 ${source.enabled ? "border-primary/20 bg-primary/[0.02]" : "opacity-70"}`}
            >
              <CardHeader className="pb-3">
                <div className="flex items-center justify-between">
                  <CardTitle className="text-sm font-medium flex items-center gap-2">
                    <IconComponent className="size-4 text-muted-foreground" />
                    {source.name}
                  </CardTitle>
                  {toggling === source.id ? (
                    <Loader2 className="size-4 animate-spin text-muted-foreground" />
                  ) : (
                    <Switch
                      id={`source-${source.id}`}
                      checked={source.enabled}
                      onCheckedChange={(checked) => handleToggle(source.id, checked)}
                    />
                  )}
                </div>
                <CardDescription className="text-xs">{source.description}</CardDescription>
              </CardHeader>
              <CardContent>
                <Badge
                  variant={source.enabled ? "outline" : "secondary"}
                  className={`text-xs ${source.enabled ? "border-emerald-500/30 text-emerald-500" : ""}`}
                >
                  {source.enabled ? "Active" : "Disabled"}
                </Badge>
              </CardContent>
            </Card>
          )
        })}
      </div>
    </div>
  )
}
