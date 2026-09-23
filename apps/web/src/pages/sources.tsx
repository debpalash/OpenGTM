import { useState, useEffect } from "react"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Switch } from "@/components/ui/switch"
import {
  Loader2, Search, MapPin, BookOpen, Briefcase, BarChart3,
  Users, Rocket, Globe, Database, Zap, Phone, Package, Star,
  UserCheck, Landmark, FileText, Newspaper, TrendingUp,
  Award, CheckCircle, Grid3X3 as Grid, Monitor, Cpu, Book,
  MessageCircle, Shield, Factory, Activity, Compass,
  List, Users2, Trophy, ShoppingBag, Store,
  BriefcaseBusiness, FolderOpen, CircleDollarSign, ClipboardList, Puzzle,
  ShieldCheck, Handshake, Code, ShoppingCart,
  type LucideIcon,
} from "lucide-react"
import { toast } from "sonner"

const API_BASE = import.meta.env.VITE_API_URL || ""

// Map icon name strings from the API to Lucide components
const ICON_MAP: Record<string, React.ComponentType<{ className?: string }>> = {
  "search": Search, "map-pin": MapPin, "book-open": BookOpen,
  "briefcase": Briefcase, "bar-chart-3": BarChart3, "users": Users,
  "rocket": Rocket, "globe": Globe, "database": Database, "zap": Zap,
  "phone": Phone, "package": Package, "star": Star, "user-check": UserCheck,
  "landmark": Landmark, "file-text": FileText, "newspaper": Newspaper,
  "trending-up": TrendingUp, "award": Award, "check-circle": CheckCircle,
  "grid": Grid, "monitor": Monitor, "cpu": Cpu, "book": Book,
  "message-circle": MessageCircle, "shield": Shield, "factory": Factory,
  "activity": Activity, "compass": Compass, "shopping-bag": ShoppingBag,
  "store": Store, "list": List, "users-2": Users2, "trophy": Trophy,
  "globe-2": Globe, "linkedin": Briefcase, "facebook": Users,
}

// Category display config
const CATEGORY_CONFIG: Record<string, { label: string; icon: LucideIcon }> = {
  "core": { label: "Core Search Engines", icon: Zap },
  "india_b2b": { label: "India — B2B Marketplaces", icon: Store },
  "india_review": { label: "India — Review & Rating Sites", icon: Star },
  "india_jobs": { label: "India — Job Portals", icon: BriefcaseBusiness },
  "india_gov": { label: "India — Government & Registry", icon: Landmark },
  "india_startup": { label: "India — Startup Ecosystem", icon: Rocket },
  "india_directory": { label: "India — More Directories", icon: FolderOpen },
  "global_tech": { label: "Global — IT/Tech Directories", icon: Monitor },
  "global_directory": { label: "Global — Business Directories", icon: BookOpen },
  "global_startup": { label: "Global — Startup & Funding", icon: CircleDollarSign },
  "global_jobs": { label: "Global — Job Boards", icon: ClipboardList },
  "saas_directory": { label: "Product / SaaS Directories", icon: Puzzle },
  "trust_review": { label: "Trust & Review Platforms", icon: ShieldCheck },
  "freelance": { label: "Freelance & Service Marketplaces", icon: Handshake },
  "developer": { label: "Developer & Tech Communities", icon: Code },
  "news": { label: "News & Media Sources", icon: Newspaper },
  "social": { label: "Social & Professional Networks", icon: Globe },
  "ecommerce": { label: "E-Commerce & Marketplace Sellers", icon: ShoppingCart },
  "europe": { label: "Europe — Directories", icon: MapPin },
  "generic": { label: "Generic Search Patterns", icon: Search },
}

interface Source {
  id: string
  name: string
  icon: string
  description: string
  enabled: boolean
  strategy: string
  category?: string
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

  const enableAll = () => {
    sources.filter(s => !s.enabled).forEach(s => handleToggle(s.id, true))
  }

  if (loading) {
    return (
      <div className="flex justify-center p-12">
        <Loader2 className="size-6 animate-spin text-muted-foreground" />
      </div>
    )
  }

  const activeCount = sources.filter(s => s.enabled).length

  // Group sources by category
  const grouped = sources.reduce<Record<string, Source[]>>((acc, source) => {
    const cat = source.category || "other"
    if (!acc[cat]) acc[cat] = []
    acc[cat].push(source)
    return acc
  }, {})

  // Order categories
  const categoryOrder = Object.keys(CATEGORY_CONFIG)
  const orderedCategories = categoryOrder.filter(cat => grouped[cat]?.length > 0)

  return (
    <div className="p-4 space-y-4 max-h-[calc(100vh-60px)] overflow-y-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Data Sources</h2>
          <p className="text-sm text-muted-foreground">
            Configure which data sources agents use for lead discovery and enrichment.
            <span className="ml-2 text-foreground font-medium">{activeCount}/{sources.length} active</span>
          </p>
        </div>
        <button
          onClick={enableAll}
          className="px-3 py-1.5 rounded-md text-xs bg-primary text-primary-foreground hover:bg-primary/90 transition-colors"
        >
          Enable All
        </button>
      </div>

      <Separator />

      {orderedCategories.map(category => {
        const config = CATEGORY_CONFIG[category]
        const catSources = grouped[category]
        const catActive = catSources.filter(s => s.enabled).length

        return (
          <div key={category}>
            <div className="flex items-center gap-2 mb-2">
              <config.icon aria-hidden="true" className="size-4 text-muted-foreground" />
              <h3 className="text-sm font-medium text-foreground">{config.label}</h3>
              <Badge variant="outline" className="text-[10px] px-1.5 py-0">
                {catActive}/{catSources.length}
              </Badge>
            </div>

            <div className="grid gap-2 md:grid-cols-2 lg:grid-cols-3 mb-4">
              {catSources.map((source) => {
                const IconComponent = ICON_MAP[source.icon] || Database
                return (
                  <div
                    key={source.id}
                    className={`flex items-center gap-3 rounded-lg border px-3 py-2.5 transition-all duration-200 ${
                      source.enabled
                        ? "border-primary/20 bg-primary/[0.02]"
                        : "opacity-50 border-border/50"
                    }`}
                  >
                    <IconComponent className="size-4 text-muted-foreground shrink-0" />
                    <div className="flex-1 min-w-0">
                      <div className="text-sm font-medium truncate">{source.name}</div>
                      <div className="text-[10px] text-muted-foreground truncate">{source.description}</div>
                    </div>
                    {toggling === source.id ? (
                      <Loader2 className="size-4 animate-spin text-muted-foreground shrink-0" />
                    ) : (
                      <Switch
                        id={`source-${source.id}`}
                        checked={source.enabled}
                        onCheckedChange={(checked) => handleToggle(source.id, checked)}
                        className="shrink-0"
                      />
                    )}
                  </div>
                )
              })}
            </div>
          </div>
        )
      })}
    </div>
  )
}
