/**
 * Template Gallery — curated, importable workbook recipes.
 *
 * Cards come from GET /api/templates/gallery; "Use template" instantiates the
 * recipe (POST /api/templates/gallery/{slug}/instantiate) in the active
 * workspace and navigates to the new workbook.
 */

import { useEffect, useMemo, useState } from "react"
import { useNavigate } from "react-router-dom"
import { toast } from "sonner"
import {
  Activity, Banknote, Briefcase, Building2, Combine, Cpu, Crown,
  LayoutTemplate, Loader2, Map, NotebookPen, Paintbrush, PenLine, Radar,
  RefreshCw, Rocket, Search, Send, ShieldCheck, ShoppingBag, Sparkles, Star,
  Swords, UploadCloud, Webhook, type LucideIcon,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"

interface RecipeCard {
  slug: string
  name: string
  description: string
  category: string
  icon: string
  columns_count: number
  column_types: Record<string, number>
}

interface GalleryResponse {
  recipes: RecipeCard[]
  categories: Record<string, { label: string; icon: string; color: string }>
  total: number
}

// Icon hints shipped in recipe YAML → lucide components.
const RECIPE_ICONS: Record<string, LucideIcon> = {
  rocket: Rocket,
  "shopping-bag": ShoppingBag,
  "building-2": Building2,
  star: Star,
  "shield-check": ShieldCheck,
  paintbrush: Paintbrush,
  combine: Combine,
  "refresh-cw": RefreshCw,
  "pen-line": PenLine,
  crown: Crown,
  "upload-cloud": UploadCloud,
  webhook: Webhook,
  swords: Swords,
  cpu: Cpu,
  map: Map,
  "notebook-pen": NotebookPen,
  radar: Radar,
  banknote: Banknote,
  briefcase: Briefcase,
}

const CATEGORY_ICONS: Record<string, LucideIcon> = {
  "list-building": Radar,
  "crm-hygiene": Sparkles,
  outbound: Send,
  research: Search,
  signals: Activity,
}

// Column-mix chips shown on each card.
const TYPE_LABELS: Record<string, string> = {
  source: "source",
  waterfall: "waterfall",
  enrichment: "enrichment",
  ai_formula: "AI",
  research: "research",
  formula: "formula",
  output: "push",
}

export default function TemplatesPage() {
  const navigate = useNavigate()
  const [gallery, setGallery] = useState<GalleryResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [category, setCategory] = useState<string | null>(null)
  const [search, setSearch] = useState("")
  const [usingSlug, setUsingSlug] = useState("")

  useEffect(() => {
    fetch("/api/templates/gallery")
      .then(r => r.json())
      .then(setGallery)
      .catch(() => toast.error("Failed to load template gallery"))
      .finally(() => setLoading(false))
  }, [])

  const recipes = useMemo(() => {
    let list = gallery?.recipes || []
    if (category) list = list.filter(r => r.category === category)
    const q = search.trim().toLowerCase()
    if (q) {
      list = list.filter(r =>
        r.name.toLowerCase().includes(q) || r.description.toLowerCase().includes(q)
      )
    }
    return list
  }, [gallery, category, search])

  const useTemplate = async (slug: string) => {
    setUsingSlug(slug)
    try {
      const res = await fetch(`/api/templates/gallery/${slug}/instantiate`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const wb = await res.json()
      toast.success("Workbook created from template")
      navigate(`/workbooks/${wb.id}`)
    } catch {
      toast.error("Failed to create workbook from template")
      setUsingSlug("")
    }
  }

  const categories = gallery?.categories || {}

  return (
    <div className="p-6 max-w-6xl mx-auto space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between gap-4 flex-wrap">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Template Gallery</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Curated workbook recipes — source lists, verify emails, personalize outreach, push to your CRM
          </p>
        </div>
        <div className="relative">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 size-4 text-muted-foreground" />
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search templates…"
            className="w-64 pl-8 pr-3 py-2 rounded-lg border bg-background text-sm focus:outline-none focus:ring-2 focus:ring-primary/50"
          />
        </div>
      </div>

      {/* Category Tabs */}
      <div className="flex items-center gap-1.5 text-xs overflow-x-auto">
        <button
          onClick={() => setCategory(null)}
          className={`inline-flex items-center gap-1 px-2.5 py-1.5 rounded-md whitespace-nowrap transition-colors ${
            category === null ? "bg-primary text-primary-foreground" : "hover:bg-muted text-muted-foreground"
          }`}
        >
          <LayoutTemplate className="size-3" />
          All
          {gallery && <span className="tabular-nums opacity-70">{gallery.total}</span>}
        </button>
        {Object.entries(categories).map(([key, cat]) => {
          const Icon = CATEGORY_ICONS[key] || LayoutTemplate
          const count = gallery?.recipes.filter(r => r.category === key).length || 0
          return (
            <button
              key={key}
              onClick={() => setCategory(key)}
              className={`inline-flex items-center gap-1 px-2.5 py-1.5 rounded-md whitespace-nowrap transition-colors ${
                category === key ? "bg-primary text-primary-foreground" : "hover:bg-muted text-muted-foreground"
              }`}
            >
              <Icon className="size-3" />
              {cat.label}
              <span className="tabular-nums opacity-70">{count}</span>
            </button>
          )
        })}
      </div>

      {/* Loading */}
      {loading && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {[1, 2, 3, 4, 5, 6].map(i => (
            <div key={i} className="rounded-xl border bg-card p-5 animate-pulse">
              <div className="h-5 w-40 bg-muted rounded mb-3" />
              <div className="h-3 w-full bg-muted rounded mb-2" />
              <div className="h-3 w-2/3 bg-muted rounded mb-4" />
              <div className="h-8 w-full bg-muted rounded" />
            </div>
          ))}
        </div>
      )}

      {/* Empty search result */}
      {!loading && recipes.length === 0 && (
        <div className="rounded-xl border-2 border-dashed bg-card/50 p-16 text-center">
          <LayoutTemplate className="size-12 mx-auto text-muted-foreground/50 mb-4" />
          <h3 className="text-lg font-medium mb-1">No templates match</h3>
          <p className="text-sm text-muted-foreground">
            Try a different search or category.
          </p>
        </div>
      )}

      {/* Recipe Grid */}
      {!loading && recipes.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {recipes.map(r => {
            const Icon = RECIPE_ICONS[r.icon] || LayoutTemplate
            const catLabel = categories[r.category]?.label || r.category
            const catColor = categories[r.category]?.color || "text-muted-foreground"
            return (
              <div
                key={r.slug}
                className="group rounded-xl border bg-card p-5 flex flex-col hover:border-primary/50 hover:shadow-md transition-all duration-200"
              >
                <div className="flex items-start justify-between mb-3">
                  <div className="p-2 rounded-lg bg-primary/10">
                    <Icon className="size-4 text-primary" />
                  </div>
                  <span className={`inline-flex items-center gap-1 text-[10px] font-medium ${catColor}`}>
                    {catLabel}
                  </span>
                </div>

                <h3 className="font-semibold text-sm mb-1.5">{r.name}</h3>
                <p className="text-xs text-muted-foreground line-clamp-3 mb-3 flex-1">
                  {r.description}
                </p>

                {/* Column mix */}
                <div className="flex items-center gap-1 flex-wrap mb-4">
                  <span className="text-[10px] text-muted-foreground mr-1">
                    {r.columns_count} columns
                  </span>
                  {Object.entries(r.column_types)
                    .filter(([t]) => t !== "lead_field")
                    .map(([t, n]) => (
                      <Badge key={t} variant="secondary" className="text-[10px] px-1.5 py-0">
                        {n}× {TYPE_LABELS[t] || t}
                      </Badge>
                    ))}
                </div>

                <button
                  onClick={() => useTemplate(r.slug)}
                  disabled={!!usingSlug}
                  className="w-full inline-flex items-center justify-center gap-2 px-3 py-2 rounded-lg bg-primary text-primary-foreground text-sm font-medium hover:bg-primary/90 disabled:opacity-50 transition-colors"
                >
                  {usingSlug === r.slug ? (
                    <>
                      <Loader2 className="size-3.5 animate-spin" />
                      Creating…
                    </>
                  ) : (
                    "Use template"
                  )}
                </button>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
