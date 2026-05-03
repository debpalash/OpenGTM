/**
 * Tambo Component Registry
 * 
 * Registers generative UI components with Tambo so the AI agent
 * can dynamically render them in chat responses.
 * 
 * Self-hosted Tambo API: http://localhost:8261
 */
import { z } from "zod"
import type { TamboComponent } from "@tambo-ai/react"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"

// ── Schemas ─────────────────────────────────────────────────────

export const LeadCardPropsSchema = z.object({
  company: z.string().describe("Company name"),
  city: z.string().describe("City/location of the company"),
  email: z.string().optional().describe("Contact email"),
  phone: z.string().optional().describe("Contact phone"),
  website: z.string().optional().describe("Company website URL"),
  score: z.number().optional().describe("Lead score 0-100"),
  tier: z.string().optional().describe("Score tier: hot, warm, or cold"),
  status: z.string().optional().describe("Lead status: new, contacted, qualified, dead"),
}).describe("Displays a compact lead summary card for a single company")

export const ScoreGaugePropsSchema = z.object({
  score: z.number().describe("Numeric score value 0-100"),
  label: z.string().describe("Label for this score (e.g. 'Lead Quality')"),
  tier: z.string().optional().describe("Tier classification: hot, warm, cold"),
}).describe("Shows a visual score gauge with progress bar and tier badge")

export const StatsOverviewPropsSchema = z.object({
  total: z.number().describe("Total number of leads"),
  hot: z.number().describe("Number of hot leads"),
  warm: z.number().describe("Number of warm leads"),
  cold: z.number().describe("Number of cold leads"),
  withEmail: z.number().optional().describe("Leads with email"),
  withPhone: z.number().optional().describe("Leads with phone"),
}).describe("Shows a summary stats overview of the lead pipeline")

// ── Components ──────────────────────────────────────────────────

type LeadCardProps = z.infer<typeof LeadCardPropsSchema>

function LeadCard({ company, city, email, phone, website, score, tier, status }: LeadCardProps) {
  const tierVariant = tier === "hot" ? "destructive" : tier === "warm" ? "default" : "secondary"
  return (
    <Card className="w-full max-w-sm">
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <CardTitle className="text-base">{company}</CardTitle>
          {tier && <Badge variant={tierVariant}>{tier}</Badge>}
        </div>
        <CardDescription>{city}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-1 text-sm">
        {email && <div className="text-muted-foreground">✉ {email}</div>}
        {phone && <div className="text-muted-foreground">☎ {phone}</div>}
        {website && (
          <a href={website} target="_blank" rel="noopener noreferrer" className="text-primary hover:underline">
            {website}
          </a>
        )}
        {score !== undefined && (
          <div className="flex items-center gap-2 pt-1">
            <Progress value={score} className="h-2 flex-1" />
            <span className="text-xs font-medium">{score}</span>
          </div>
        )}
        {status && <Badge variant="outline" className="mt-1">{status}</Badge>}
      </CardContent>
    </Card>
  )
}

type ScoreGaugeProps = z.infer<typeof ScoreGaugePropsSchema>

function ScoreGauge({ score, label, tier }: ScoreGaugeProps) {
  const tierVariant = tier === "hot" ? "destructive" : tier === "warm" ? "default" : "secondary"
  return (
    <div className="flex flex-col gap-2 rounded-lg border p-4 w-full max-w-xs">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium">{label}</span>
        {tier && <Badge variant={tierVariant}>{tier}</Badge>}
      </div>
      <Progress value={score} className="h-3" />
      <span className="text-2xl font-bold tabular-nums">{score}/100</span>
    </div>
  )
}

type StatsOverviewProps = z.infer<typeof StatsOverviewPropsSchema>

function StatsOverview({ total, hot, warm, cold, withEmail, withPhone }: StatsOverviewProps) {
  return (
    <div className="grid grid-cols-3 gap-3 w-full max-w-md">
      <Card>
        <CardContent className="p-3 text-center">
          <div className="text-2xl font-bold">{total}</div>
          <div className="text-xs text-muted-foreground">Total</div>
        </CardContent>
      </Card>
      <Card>
        <CardContent className="p-3 text-center">
          <div className="text-2xl font-bold text-destructive">{hot}</div>
          <div className="text-xs text-muted-foreground">Hot</div>
        </CardContent>
      </Card>
      <Card>
        <CardContent className="p-3 text-center">
          <div className="text-2xl font-bold">{warm}</div>
          <div className="text-xs text-muted-foreground">Warm</div>
        </CardContent>
      </Card>
      <Card>
        <CardContent className="p-3 text-center">
          <div className="text-2xl font-bold text-muted-foreground">{cold}</div>
          <div className="text-xs text-muted-foreground">Cold</div>
        </CardContent>
      </Card>
      {withEmail !== undefined && (
        <Card>
          <CardContent className="p-3 text-center">
            <div className="text-2xl font-bold">{withEmail}</div>
            <div className="text-xs text-muted-foreground">With Email</div>
          </CardContent>
        </Card>
      )}
      {withPhone !== undefined && (
        <Card>
          <CardContent className="p-3 text-center">
            <div className="text-2xl font-bold">{withPhone}</div>
            <div className="text-xs text-muted-foreground">With Phone</div>
          </CardContent>
        </Card>
      )}
    </div>
  )
}

// ── Registration ────────────────────────────────────────────────

export const tamboComponents: TamboComponent[] = [
  {
    component: LeadCard,
    name: "LeadCard",
    description: "Displays a compact lead summary card. Use when showing a single lead's info.",
    propsSchema: LeadCardPropsSchema,
  },
  {
    component: ScoreGauge,
    name: "ScoreGauge",
    description: "Shows a visual score gauge with progress bar. Use when displaying a score or rating.",
    propsSchema: ScoreGaugePropsSchema,
  },
  {
    component: StatsOverview,
    name: "StatsOverview",
    description: "Shows pipeline stats overview. Use when summarizing the entire lead pipeline.",
    propsSchema: StatsOverviewPropsSchema,
  },
]
