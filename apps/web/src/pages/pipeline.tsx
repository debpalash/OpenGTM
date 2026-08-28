import React, { useState } from "react"
import { toast } from "sonner"
import {
  CheckCircle2, XCircle, Clock, Loader2, ChevronDown,
  Play, Workflow, Brain,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { ScrollArea } from "@/components/ui/scroll-area"
import { useJobs, useCollect } from "@/lib/hooks"
import type { Job } from "@/lib/api"

const STATUS_ICON: Record<string, typeof CheckCircle2> = {
  done: CheckCircle2,
  running: Loader2,
  pending: Clock,
  failed: XCircle,
}

const STATUS_COLORS: Record<string, string> = {
  done: "text-green-500",
  running: "text-blue-500",
  pending: "text-muted-foreground",
  failed: "text-destructive",
}

function JobCard({ job, isSelected, onClick }: { job: Job; isSelected: boolean; onClick: () => void }) {
  const Icon = STATUS_ICON[job.status] || Clock
  const isRunning = job.status === "running"

  return (
    <button
      onClick={onClick}
      className={`w-full text-left p-3 rounded-lg border transition-colors ${
        isSelected ? "bg-accent border-border" : "hover:bg-muted/50 border-transparent"
      }`}
    >
      <div className="flex items-start gap-2">
        <Icon className={`size-4 mt-0.5 shrink-0 ${STATUS_COLORS[job.status]} ${isRunning ? "animate-spin" : ""}`} />
        <div className="flex-1 min-w-0">
          <div className="text-sm font-medium truncate">{job.query}</div>
          <div className="flex items-center gap-2 mt-1">
            <Badge variant="outline" className="text-xs">
              {job.leads_found} leads
            </Badge>
            <span className="text-xs text-muted-foreground">
              {new Date(job.created_at).toLocaleTimeString()}
            </span>
          </div>
        </div>
      </div>
    </button>
  )
}

interface JobStage {
  id: number
  job_id: string
  stage: string
  status: string
  input_count: number
  output_count: number
  rejected_count: number
  details: string
  started_at: string
  completed_at: string
}

function StageCard({ stage }: { stage: JobStage }) {
  const Icon = STATUS_ICON[stage.status] || Clock
  const isRunning = stage.status === "running"
  let details: Record<string, unknown> = {}
  try { details = JSON.parse(stage.details || "{}") } catch { /* ignore */ }

  const isAI = !!details.ai
  const tokens = details.tokens as Record<string, number | string> | undefined
  const tiers = details.tiers as Record<string, number> | undefined
  const reasons = details.reasons as Record<string, number> | undefined
  const samples = details.samples as string[] | undefined
  const error = details.error as string | undefined

  return (
    <Collapsible>
      <CollapsibleTrigger className="w-full flex items-center gap-3 p-3 rounded-lg hover:bg-muted/50 transition-colors text-left">
        <Icon className={`size-4 shrink-0 ${STATUS_COLORS[stage.status]} ${isRunning ? "animate-spin" : ""}`} />
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium capitalize">{stage.stage}</span>
            {isAI && (
              <Badge variant="secondary" className="text-xs gap-1">
                <Brain className="size-3" /> AI
              </Badge>
            )}
          </div>
          <div className="text-xs text-muted-foreground mt-0.5">
            {stage.input_count > 0 && `${stage.input_count} in → `}
            {stage.output_count} out
            {stage.rejected_count > 0 && ` · ${stage.rejected_count} rejected`}
          </div>
        </div>
        <ChevronDown className="size-4 text-muted-foreground" />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="px-3 pb-3 ml-7 space-y-2">
          {tiers && (
            <div className="flex gap-2 text-xs">
              {Object.entries(tiers).map(([t, count]) => (
                <Badge key={t} variant="outline" className="text-xs">
                  {t}: {count}
                </Badge>
              ))}
            </div>
          )}
          {reasons && (
            <div className="text-xs text-muted-foreground space-y-0.5">
              {Object.entries(reasons).map(([reason, count]) => (
                <div key={reason}>{reason}: {count}</div>
              ))}
            </div>
          )}
          {tokens && (
            <div className="text-xs text-muted-foreground">
              Tokens: {tokens.total_tokens ?? 0} ({tokens.provider ?? "—"}) · {tokens.calls ?? 0} calls
            </div>
          )}
          {samples && (
            <div className="text-xs text-muted-foreground">
              Samples: {samples.slice(0, 5).join(", ")}
            </div>
          )}
          {error && (
            <div className="text-xs text-destructive">{error}</div>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

export default function PipelinePage() {
  const [selectedJobId, setSelectedJobId] = useState<string | null>(null)
  const [collectQuery, setCollectQuery] = useState("")
  const [stages, setStages] = useState<JobStage[]>([])
  const { data: jobs, isLoading } = useJobs()
  const collect = useCollect()

  const selectedJob = jobs?.find(j => j.id === selectedJobId) || null

  const loadStages = async (jobId: string) => {
    try {
      const res = await fetch(`/api/jobs/${jobId}/stages`)
      const data = await res.json()
      setStages(data.stages || [])
    } catch {
      setStages([])
    }
  }

  // Auto-refresh stages every 3s for running jobs
  React.useEffect(() => {
    if (!selectedJob || selectedJob.status !== "running") return
    const interval = setInterval(() => loadStages(selectedJob.id), 3000)
    return () => clearInterval(interval)
  }, [selectedJob?.id, selectedJob?.status])

  // Auto-select newly created running job
  React.useEffect(() => {
    if (!jobs?.length) return
    const running = jobs.find(j => j.status === "running")
    if (running && !selectedJobId) {
      setSelectedJobId(running.id)
      loadStages(running.id)
    }
  }, [jobs])

  const handleSelectJob = (job: Job) => {
    setSelectedJobId(job.id)
    loadStages(job.id)
  }

  const handleCollect = () => {
    if (!collectQuery.trim()) return
    const query = collectQuery.trim()
    collect.mutate({ query }, {
      onSuccess: (result) => {
        if (!result.ok) return
        toast.success(`Pipeline started: "${query}"`)
        setCollectQuery("")
        setSelectedJobId(result.job_id)
      },
      onError: (error) => toast.error(error.message || "Could not start pipeline"),
    })
  }

  const runningJobs = jobs?.filter(j => j.status === "running") ?? []
  const completedJobs = jobs?.filter(j => j.status === "done") ?? []
  const failedJobs = jobs?.filter(j => j.status === "failed") ?? []

  return (
    <div className="flex h-full">
      {/* Job List Sidebar */}
      <div className="w-80 border-r flex flex-col shrink-0">
        <div className="p-3 border-b space-y-2">
          <div className="flex items-center gap-1">
            <Input
              placeholder="New collection query..."
              value={collectQuery}
              onChange={(e) => setCollectQuery(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && handleCollect()}
            />
            <Button size="sm" onClick={handleCollect} disabled={collect.isPending}>
              <Play className="size-4" />
            </Button>
          </div>
        </div>

        <ScrollArea className="flex-1">
          <div className="p-2 space-y-1">
            {isLoading ? (
              Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-16 w-full" />)
            ) : (
              <>
                {runningJobs.length > 0 && (
                  <>
                    <div className="px-2 py-1 text-xs font-medium text-muted-foreground uppercase">Running</div>
                    {runningJobs.map(j => (
                      <JobCard key={j.id} job={j} isSelected={j.id === selectedJobId} onClick={() => handleSelectJob(j)} />
                    ))}
                  </>
                )}
                {completedJobs.length > 0 && (
                  <>
                    <div className="px-2 py-1 text-xs font-medium text-muted-foreground uppercase mt-2">Completed</div>
                    {completedJobs.map(j => (
                      <JobCard key={j.id} job={j} isSelected={j.id === selectedJobId} onClick={() => handleSelectJob(j)} />
                    ))}
                  </>
                )}
                {failedJobs.length > 0 && (
                  <>
                    <div className="px-2 py-1 text-xs font-medium text-muted-foreground uppercase mt-2">Failed</div>
                    {failedJobs.map(j => (
                      <JobCard key={j.id} job={j} isSelected={j.id === selectedJobId} onClick={() => handleSelectJob(j)} />
                    ))}
                  </>
                )}
                {(!jobs || jobs.length === 0) && (
                  <div className="text-center text-sm text-muted-foreground py-8">
                    No jobs yet. Start a collection above.
                  </div>
                )}
              </>
            )}
          </div>
        </ScrollArea>
      </div>

      {/* Job Detail Panel */}
      <div className="flex-1 overflow-auto">
        {selectedJob ? (
          <div className="p-6 max-w-2xl mx-auto space-y-6">
            <div>
              <h2 className="text-lg font-semibold">{selectedJob.query}</h2>
              <div className="flex items-center gap-2 mt-1">
                <Badge variant={selectedJob.status === "done" ? "default" : "secondary"}>
                  {selectedJob.status}
                </Badge>
                <span className="text-sm text-muted-foreground">
                  {selectedJob.leads_found} leads found
                </span>
                <span className="text-xs text-muted-foreground">
                  · {new Date(selectedJob.created_at).toLocaleString()}
                </span>
              </div>
            </div>

            <Separator />

            <div className="space-y-1">
              <h3 className="text-sm font-medium mb-3 flex items-center gap-2">
                <Workflow className="size-4" />
                Pipeline Stages
              </h3>
              {stages.length > 0 ? (
                stages.map(s => <StageCard key={s.id} stage={s} />)
              ) : (
                <div className="text-sm text-muted-foreground py-4 text-center">
                  Loading stages...
                </div>
              )}
            </div>

            {selectedJob.error && (
              <>
                <Separator />
                <div className="text-sm text-destructive">
                  <strong>Error:</strong> {selectedJob.error}
                </div>
              </>
            )}
          </div>
        ) : (
          <div className="flex items-center justify-center h-full text-muted-foreground">
            <div className="text-center space-y-2">
              <Workflow className="size-12 mx-auto opacity-20" />
              <p className="text-sm">Select a job to view pipeline details</p>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
