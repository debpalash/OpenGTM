
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Switch } from "@/components/ui/switch"

const SOURCES = [
  { id: "duckduckgo", name: "DuckDuckGo", icon: "🦆", description: "Web search for company websites", enabled: true },
  { id: "google_maps", name: "Google Maps", icon: "📍", description: "Local business listings with reviews", enabled: true },
  { id: "directories", name: "Business Directories", icon: "📒", description: "Clutch, GoodFirms, JustDial, etc.", enabled: true },
  { id: "linkedin", name: "LinkedIn", icon: "💼", description: "Professional network profiles", enabled: false },
  { id: "ambitionbox", name: "AmbitionBox", icon: "📊", description: "Company reviews, salaries, interviews", enabled: false },
  { id: "crunchbase", name: "Crunchbase", icon: "🚀", description: "Startup funding and company data", enabled: false },
]

export default function SourcesPage() {
  return (
    <div className="p-6 max-w-4xl mx-auto space-y-6">
      <div>
        <h2 className="text-lg font-semibold">Data Sources</h2>
        <p className="text-sm text-muted-foreground">
          Configure which data sources agents use for lead discovery and enrichment.
        </p>
      </div>

      <Separator />

      <div className="grid gap-4 md:grid-cols-2">
        {SOURCES.map((source) => (
          <Card key={source.id}>
            <CardHeader className="pb-3">
              <div className="flex items-center justify-between">
                <CardTitle className="text-sm font-medium flex items-center gap-2">
                  <span className="text-lg">{source.icon}</span>
                  {source.name}
                </CardTitle>
                <Switch id={`source-${source.id}`} defaultChecked={source.enabled} />
              </div>
              <CardDescription className="text-xs">{source.description}</CardDescription>
            </CardHeader>
            <CardContent>
              <Badge variant={source.enabled ? "outline" : "secondary"} className="text-xs">
                {source.enabled ? "Active" : "Not configured"}
              </Badge>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  )
}
