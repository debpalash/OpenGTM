import {
  Activity, CheckCircle2, XCircle,
  Clock, Loader2,
} from "lucide-react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"
import { Switch } from "@/components/ui/switch"
import { Skeleton } from "@/components/ui/skeleton"
import { ScrollArea } from "@/components/ui/scroll-area"
import { useJobs } from "@/lib/hooks"

const AGENT_DEFS = [
  {
    id: "source_agent",
    name: "Source Agent",
    description: "Searches DDG, Maps, and directories for company URLs",
    icon: "🔍",
  },
  {
    id: "enrichment_agent",
    name: "Enrichment Agent",
    description: "Extracts company data, emails, phones from websites using AI",
    icon: "🤖",
  },
  {
    id: "scoring_agent",
    name: "Scoring Agent",
    description: "Scores leads against your ICP using LLM reasoning",
    icon: "📊",
  },
  {
    id: "signal_agent",
    name: "Signal Agent",
    description: "Monitors hiring, funding, and growth signals",
    icon: "📡",
  },
  {
    id: "outreach_agent",
    name: "Outreach Agent",
    description: "Generates and sends personalized outreach messages",
    icon: "✉️",
  },
]

const STATUS_ICON: Record<string, typeof CheckCircle2> = {
  done: CheckCircle2,
  running: Loader2,
  pending: Clock,
  failed: XCircle,
}

const STATUS_COLOR: Record<string, string> = {
  done: "text-green-500",
  running: "text-blue-500",
  pending: "text-muted-foreground",
  failed: "text-destructive",
}

export default function AgentsPage() {
  const { data: jobs, isLoading } = useJobs()

  const runningJobs = jobs?.filter(j => j.status === "running") ?? []
  const recentJobs = jobs?.slice(0, 10) ?? []

  return (
    <div className="flex h-full">
      {/* Agent Definitions */}
      <div className="flex-1 p-6 overflow-auto">
        <div className="max-w-3xl mx-auto space-y-6">
          <div>
            <h2 className="text-lg font-semibold">Agents</h2>
            <p className="text-sm text-muted-foreground">
              Control your AI agents. Each agent handles a specific stage of the lead pipeline.
            </p>
          </div>

          <Separator />

          <div className="grid gap-4 md:grid-cols-2">
            {AGENT_DEFS.map((agent) => (
              <Card key={agent.id}>
                <CardHeader className="pb-3">
                  <div className="flex items-center justify-between">
                    <CardTitle className="text-sm font-medium flex items-center gap-2">
                      <span className="text-lg">{agent.icon}</span>
                      {agent.name}
                    </CardTitle>
                    <div className="flex items-center gap-2">
                      <Switch
                        id={`agent-${agent.id}`}
                        defaultChecked={["source_agent", "enrichment_agent", "scoring_agent"].includes(agent.id)}
                      />
                    </div>
                  </div>
                  <CardDescription className="text-xs">
                    {agent.description}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <div className="flex items-center gap-2">
                    <Badge variant="outline" className="text-xs">
                      {["source_agent", "enrichment_agent", "scoring_agent"].includes(agent.id) ? "Active" : "Idle"}
                    </Badge>
                    {runningJobs.length > 0 && ["source_agent", "enrichment_agent", "scoring_agent"].includes(agent.id) && (
                      <Badge variant="secondary" className="text-xs gap-1">
                        <Loader2 className="size-3 animate-spin" />
                        Working
                      </Badge>
                    )}
                  </div>
                </CardContent>
              </Card>
            ))}
          </div>
        </div>
      </div>

      {/* Job Queue Sidebar */}
      <div className="w-72 border-l flex flex-col shrink-0">
        <div className="p-3 border-b">
          <h3 className="text-sm font-medium flex items-center gap-2">
            <Activity className="size-4" />
            Task Queue
          </h3>
        </div>
        <ScrollArea className="flex-1">
          <div className="p-2 space-y-1">
            {isLoading ? (
              Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-14 w-full" />)
            ) : recentJobs.length > 0 ? (
              recentJobs.map((job) => {
                const Icon = STATUS_ICON[job.status] || Clock
                return (
                  <div key={job.id} className="p-2 rounded-lg hover:bg-muted/50 transition-colors">
                    <div className="flex items-start gap-2">
                      <Icon className={`size-4 mt-0.5 shrink-0 ${STATUS_COLOR[job.status]} ${job.status === "running" ? "animate-spin" : ""}`} />
                      <div className="flex-1 min-w-0">
                        <div className="text-xs font-medium truncate">{job.query}</div>
                        <div className="text-xs text-muted-foreground">
                          {job.leads_found} leads · {new Date(job.created_at).toLocaleTimeString()}
                        </div>
                      </div>
                    </div>
                  </div>
                )
              })
            ) : (
              <div className="text-center text-xs text-muted-foreground py-8">
                No tasks. Start a collection from Chat.
              </div>
            )}
          </div>
        </ScrollArea>
      </div>
    </div>
  )
}
