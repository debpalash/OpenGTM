import { useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  PageHeader,
  ErrorState,
  ViewState
} from "@/design-system/patterns/workbench"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription
} from "@/components/ui/dialog"
import { getActiveWorkspace } from "@/lib/auth"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"
import {
  Loader2,
  Search,
  MapPin,
  BookOpen,
  Briefcase,
  BarChart3,
  Users,
  Rocket,
  Globe,
  Database,
  Zap,
  Phone,
  Package,
  Star,
  UserCheck,
  Landmark,
  FileText,
  Newspaper,
  TrendingUp,
  Award,
  CheckCircle,
  Grid3X3 as Grid,
  Monitor,
  Cpu,
  Book,
  MessageCircle,
  Shield,
  Factory,
  Activity,
  Compass,
  List,
  Users2,
  Trophy,
  ShoppingBag,
  Store,
  BriefcaseBusiness,
  FolderOpen,
  CircleDollarSign,
  ClipboardList,
  Puzzle,
  ShieldCheck,
  Handshake,
  Code,
  ShoppingCart,
  type LucideIcon
} from "lucide-react"
import { toast } from "sonner"

const API_BASE = import.meta.env.VITE_API_URL || ""

// Map icon name strings from the API to Lucide components
const ICON_MAP: Record<string, React.ComponentType<{ className?: string }>> = {
  search: Search,
  "map-pin": MapPin,
  "book-open": BookOpen,
  briefcase: Briefcase,
  "bar-chart-3": BarChart3,
  users: Users,
  rocket: Rocket,
  globe: Globe,
  database: Database,
  zap: Zap,
  phone: Phone,
  package: Package,
  star: Star,
  "user-check": UserCheck,
  landmark: Landmark,
  "file-text": FileText,
  newspaper: Newspaper,
  "trending-up": TrendingUp,
  award: Award,
  "check-circle": CheckCircle,
  grid: Grid,
  monitor: Monitor,
  cpu: Cpu,
  book: Book,
  "message-circle": MessageCircle,
  shield: Shield,
  factory: Factory,
  activity: Activity,
  compass: Compass,
  "shopping-bag": ShoppingBag,
  store: Store,
  list: List,
  "users-2": Users2,
  trophy: Trophy,
  "globe-2": Globe,
  linkedin: Briefcase,
  facebook: Users
}

// Category display config
const CATEGORY_CONFIG: Record<string, { label: string; icon: LucideIcon }> = {
  core: { label: "Core Search Engines", icon: Zap },
  india_b2b: { label: "India — B2B Marketplaces", icon: Store },
  india_review: { label: "India — Review & Rating Sites", icon: Star },
  india_jobs: { label: "India — Job Portals", icon: BriefcaseBusiness },
  india_gov: { label: "India — Government & Registry", icon: Landmark },
  india_startup: { label: "India — Startup Ecosystem", icon: Rocket },
  india_directory: { label: "India — More Directories", icon: FolderOpen },
  global_tech: { label: "Global — IT/Tech Directories", icon: Monitor },
  global_directory: { label: "Global — Business Directories", icon: BookOpen },
  global_startup: {
    label: "Global — Startup & Funding",
    icon: CircleDollarSign
  },
  global_jobs: { label: "Global — Job Boards", icon: ClipboardList },
  saas_directory: { label: "Product / SaaS Directories", icon: Puzzle },
  trust_review: { label: "Trust & Review Platforms", icon: ShieldCheck },
  freelance: { label: "Freelance & Service Marketplaces", icon: Handshake },
  developer: { label: "Developer & Tech Communities", icon: Code },
  news: { label: "News & Media Sources", icon: Newspaper },
  social: { label: "Social & Professional Networks", icon: Globe },
  ecommerce: { label: "E-Commerce & Marketplace Sellers", icon: ShoppingCart },
  europe: { label: "Europe — Directories", icon: MapPin },
  generic: { label: "Generic Search Patterns", icon: Search }
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
  const workspace = getActiveWorkspace()
  const client = useQueryClient()
  const query = useQuery({
    queryKey: ["source-settings", workspace],
    queryFn: async ({ signal }) => {
      const response = await fetch(`${API_BASE}/api/settings/sources`, {
        signal
      })
      if (!response.ok) throw new Error("Could not load data sources.")
      const value = await response.json()
      if (!Array.isArray(value))
        throw new Error("Data sources returned an invalid response.")
      return value as Source[]
    }
  })
  const sources = query.data ?? []
  const [search, setSearch] = useState("")
  const [status, setStatus] = useState("all")
  const [toggling, setToggling] = useState<Set<string>>(new Set())
  const [confirmAll, setConfirmAll] = useState(false)
  const [bulkBusy, setBulkBusy] = useState(false)
  async function handleToggle(
    sourceId: string,
    enabled: boolean,
    quiet = false
  ) {
    setToggling((previous) => new Set(previous).add(sourceId))
    try {
      const response = await fetch(
        `${API_BASE}/api/settings/sources/${sourceId}?enabled=${enabled}`,
        { method: "PUT" }
      )
      if (!response.ok)
        throw new Error(
          `Could not ${enabled ? "enable" : "disable"} this source (${response.status}).`
        )
      client.setQueryData<Source[]>(
        ["source-settings", workspace],
        (previous) =>
          previous?.map((source) =>
            source.id === sourceId ? { ...source, enabled } : source
          )
      )
      if (!quiet)
        toast.success(
          `${sources.find((source) => source.id === sourceId)?.name || sourceId} ${enabled ? "enabled" : "disabled"}`
        )
      return true
    } catch (cause) {
      if (!quiet)
        toast.error(
          cause instanceof Error ? cause.message : "Could not save source."
        )
      return false
    } finally {
      setToggling((previous) => {
        const next = new Set(previous)
        next.delete(sourceId)
        return next
      })
    }
  }
  const visible = sources.filter(
    (source) =>
      (status === "all" || source.enabled === (status === "enabled")) &&
      `${source.name} ${source.description}`
        .toLowerCase()
        .includes(search.toLowerCase())
  )
  async function enableAll() {
    setBulkBusy(true)
    let succeeded = 0
    const disabled = visible.filter((source) => !source.enabled)
    for (const source of disabled)
      if (await handleToggle(source.id, true, true)) succeeded++
    setBulkBusy(false)
    setConfirmAll(false)
    toast[succeeded === disabled.length ? "success" : "error"](
      `${succeeded} of ${disabled.length} sources enabled${succeeded < disabled.length ? ". Review the remaining disabled sources and retry." : "."}`
    )
  }
  const activeCount = sources.filter((s) => s.enabled).length

  // Group sources by category
  const grouped = visible.reduce<Record<string, Source[]>>((acc, source) => {
    const cat = source.category || "other"
    if (!acc[cat]) acc[cat] = []
    acc[cat].push(source)
    return acc
  }, {})

  // Order categories
  const categoryOrder = Object.keys(CATEGORY_CONFIG)
  const orderedCategories = [
    ...categoryOrder,
    ...Object.keys(grouped).filter(
      (category) => !categoryOrder.includes(category)
    )
  ].filter((cat) => grouped[cat]?.length > 0)

  return (
    <div className="workbench-page">
      <PageHeader
        title="Data sources"
        description="Choose where your agents discover and enrich records."
        actions={
          <Button
            variant="outline"
            disabled={
              query.isPending ||
              !visible.some((source) => !source.enabled) ||
              bulkBusy
            }
            onClick={() => setConfirmAll(true)}
          >
            Enable matching sources
          </Button>
        }
      />
      <div className="workbench-toolbar">
        <div className="workbench-search">
          <Search aria-hidden="true" />
          <Input
            type="search"
            aria-label="Search data sources"
            placeholder="Search data sources…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </div>
        <select
          aria-label="Filter source status"
          value={status}
          onChange={(event) => setStatus(event.target.value)}
          className="h-9 rounded-md border border-input px-3 text-sm"
        >
          <option value="all">All statuses</option>
          <option value="enabled">Enabled</option>
          <option value="disabled">Disabled</option>
        </select>
        <span role="status" className="text-xs text-muted-foreground">
          {query.data
            ? `${visible.length} shown · ${activeCount} of ${sources.length} enabled`
            : "Loading sources…"}
        </span>
      </div>
      {query.isPending ? (
        <ViewState kind="loading" title="Loading data sources…" />
      ) : query.isError ? (
        <ErrorState error={query.error} retry={() => void query.refetch()} />
      ) : !visible.length ? (
        <ViewState
          title="No matching sources"
          description="Try another name or change the status filter."
        />
      ) : null}
      <div className="p-4 md:p-6 space-y-6">
        {orderedCategories.map((category) => {
          const config = CATEGORY_CONFIG[category] || {
            label: category.replaceAll("_", " "),
            icon: Database
          }
          const catSources = grouped[category]
          const catActive = catSources.filter((s) => s.enabled).length

          return (
            <div key={category}>
              <div className="flex items-center gap-2 mb-2">
                <config.icon
                  aria-hidden="true"
                  className="size-4 text-muted-foreground"
                />
                <h2 className="text-sm font-medium text-foreground">
                  {config.label}
                </h2>
                <Badge variant="outline" className="text-xs px-1.5 py-0">
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
                          : "border-border bg-background"
                      }`}
                    >
                      <IconComponent className="size-4 text-muted-foreground shrink-0" />
                      <div className="flex-1 min-w-0">
                        <div className="text-sm font-medium truncate">
                          {source.name}
                        </div>
                        <div className="text-xs text-muted-foreground truncate">
                          {source.description}
                        </div>
                      </div>
                      <Switch
                        id={`source-${source.id}`}
                        aria-label={`${source.name} data source`}
                        aria-busy={toggling.has(source.id) || undefined}
                        disabled={toggling.has(source.id) || bulkBusy}
                        checked={source.enabled}
                        onCheckedChange={(checked) =>
                          void handleToggle(source.id, checked)
                        }
                        className="shrink-0"
                      />
                    </div>
                  )
                })}
              </div>
            </div>
          )
        })}
      </div>
      <Dialog
        open={confirmAll}
        onOpenChange={(open) => {
          if (!bulkBusy) setConfirmAll(open)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              Enable {visible.filter((source) => !source.enabled).length}{" "}
              matching sources?
            </DialogTitle>
            <DialogDescription>
              These sources become available to future discovery and enrichment
              jobs. Credentials and provider limits still apply.
            </DialogDescription>
          </DialogHeader>
          <div className="flex justify-end gap-2">
            <Button
              variant="outline"
              disabled={bulkBusy}
              onClick={() => setConfirmAll(false)}
            >
              Cancel
            </Button>
            <Button disabled={bulkBusy} onClick={() => void enableAll()}>
              {bulkBusy ? <Loader2 className="animate-spin" /> : null}Enable
              sources
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
