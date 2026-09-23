/**
 * Everything the chat can start: slash commands, the action pills above the
 * composer and the suggestion cards on the empty state.
 *
 * A `prompt` action fills the composer (the first [placeholder] is selected so
 * the user can type over it); `run: true` sends it straight away. A `go`
 * action navigates to a page instead.
 */
import {
  BarChart3, Building2, Eye, Flame, Mail, Megaphone, Plug, Plus, Radar,
  Search, Settings, Sparkles, Table2, Target, Upload, Users, Workflow,
  type LucideIcon,
} from "lucide-react"

export type ChatAction =
  | { kind: "prompt"; text: string; run?: boolean }
  | { kind: "go"; to: string }

export interface SlashCommand {
  name: string
  label: string
  hint: string
  icon: LucideIcon
  group: "Find" | "Work" | "Go to"
  action: ChatAction
}

export const SLASH_COMMANDS: SlashCommand[] = [
  { name: "find", label: "Find companies", hint: "Build an account list from a description", icon: Search, group: "Find",
    action: { kind: "prompt", text: "Find [50] companies that [are B2B SaaS in Germany with 50-200 employees]" } },
  { name: "people", label: "Find people", hint: "Decision makers at target accounts", icon: Users, group: "Find",
    action: { kind: "prompt", text: "Find the [heads of sales] at [stripe.com]" } },
  { name: "research", label: "Research a company", hint: "Deep brief with sources", icon: Building2, group: "Find",
    action: { kind: "prompt", text: "Research [company.com]: what they sell, who buys it, recent news and GTM team" } },
  { name: "signals", label: "Scan for signals", hint: "Hiring, funding, job changes", icon: Radar, group: "Find",
    action: { kind: "prompt", text: "Which of my accounts show buying signals this month (hiring, funding, leadership changes)?" } },
  { name: "enrich", label: "Enrich leads", hint: "Fill emails, phones and firmographics", icon: Sparkles, group: "Work",
    action: { kind: "prompt", text: "Enrich my [hot] leads that are missing an email" } },
  { name: "audience", label: "Build an audience", hint: "Segment leads for a campaign", icon: Target, group: "Work",
    action: { kind: "prompt", text: "Build an audience of [founders at seed-stage fintechs] from my leads" } },
  { name: "outreach", label: "Draft outreach", hint: "A short, personal sequence", icon: Mail, group: "Work",
    action: { kind: "prompt", text: "Draft a 3-step email sequence for [audience] about [our offer]" } },
  { name: "watch", label: "Watch a company", hint: "Get told when something changes", icon: Eye, group: "Work",
    action: { kind: "prompt", text: "Watch [company.com] and tell me about new hires and funding" } },
  { name: "stats", label: "Pipeline stats", hint: "Where the pipeline stands", icon: BarChart3, group: "Work",
    action: { kind: "prompt", text: "Show me my pipeline stats", run: true } },
  { name: "hot", label: "Hot leads", hint: "Leads worth a call today", icon: Flame, group: "Work",
    action: { kind: "prompt", text: "Which hot leads should I contact today, and why?", run: true } },
  { name: "workbook", label: "Workbooks", hint: "Tables that enrich themselves", icon: Table2, group: "Go to", action: { kind: "go", to: "/workbooks" } },
  { name: "import", label: "Import a CSV", hint: "Open a workbook, then import a file", icon: Upload, group: "Go to", action: { kind: "go", to: "/workbooks" } },
  { name: "leads", label: "Leads", hint: "Every lead in this workspace", icon: Users, group: "Go to", action: { kind: "go", to: "/leads" } },
  { name: "campaigns", label: "Campaigns", hint: "Sequences and sends", icon: Megaphone, group: "Go to", action: { kind: "go", to: "/campaigns" } },
  { name: "automations", label: "Automations", hint: "Rules that run on new rows", icon: Workflow, group: "Go to", action: { kind: "go", to: "/automations" } },
  { name: "sources", label: "Sources", hint: "Providers and connectors", icon: Plug, group: "Go to", action: { kind: "go", to: "/sources" } },
  { name: "settings", label: "Settings", hint: "Workspace, keys and team", icon: Settings, group: "Go to", action: { kind: "go", to: "/settings" } },
  { name: "new", label: "New chat", hint: "Start over", icon: Plus, group: "Go to", action: { kind: "go", to: "/chat" } },
]

/** Match on name first, then label/hint; an empty query lists everything. */
export function matchCommands(query: string): SlashCommand[] {
  const q = query.trim().toLowerCase()
  if (!q) return SLASH_COMMANDS
  const starts = SLASH_COMMANDS.filter(c => c.name.startsWith(q))
  const words = (c: SlashCommand) => `${c.label} ${c.hint}`.toLowerCase().split(/[^a-z0-9]+/)
  const rest = SLASH_COMMANDS.filter(c => !starts.includes(c) && words(c).some(w => w.startsWith(q)))
  return [...starts, ...rest]
}

/** Pills above the composer: one per core job, each a slash command. */
export const PILLS = ["find", "people", "research", "enrich", "signals", "audience", "outreach", "import"]
  .map(name => SLASH_COMMANDS.find(c => c.name === name)!)

export interface Suggestion { title: string; body: string; icon: LucideIcon; tone: string; action: ChatAction }

export const SUGGESTIONS: Suggestion[] = [
  { title: "Build a target list", body: "50 IT staffing firms in Pune, with founders' emails", icon: Search, tone: "sky",
    action: { kind: "prompt", text: "Build a list of 50 IT staffing firms in Pune and find their founders' emails", run: true } },
  { title: "Who to call today", body: "Hot leads ranked by fit and recent activity", icon: Flame, tone: "amber",
    action: { kind: "prompt", text: "Which hot leads should I contact today, and why?", run: true } },
  { title: "Fill the gaps", body: "Enrich hot leads that have no email yet", icon: Sparkles, tone: "violet",
    action: { kind: "prompt", text: "Find hot leads missing email and enrich them", run: true } },
  { title: "Spot buying signals", body: "Hiring, funding and leadership changes", icon: Radar, tone: "emerald",
    action: { kind: "prompt", text: "Which of my accounts show buying signals this month (hiring, funding, leadership changes)?", run: true } },
  { title: "Research an account", body: "A sourced brief before your next call", icon: Building2, tone: "rose",
    action: { kind: "prompt", text: "Research [company.com]: what they sell, who buys it, recent news and GTM team" } },
  { title: "Start from a CSV", body: "Upload a list and enrich it in a workbook", icon: Upload, tone: "slate",
    action: { kind: "go", to: "/workbooks" } },
]

/** Select the first [placeholder] so typing replaces it. */
export function placeholderRange(text: string): [number, number] | null {
  const start = text.indexOf("[")
  const end = text.indexOf("]", start)
  return start >= 0 && end > start ? [start, end + 1] : null
}
