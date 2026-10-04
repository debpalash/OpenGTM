import { getRouteApi, useNavigate } from "@tanstack/react-router"
import { ArrowLeft } from "lucide-react"
import { Button } from "@/components/ui/button"
import { TaskDetailCard } from "@/components/task-detail-card"

const route = getRouteApi("/_app/agents/$jobId")

export default function TaskDetailPage() {
  const { jobId } = route.useParams()
  const navigate = useNavigate()

  if (!jobId) {
    return (
      <div className="flex items-center justify-center h-full text-muted-foreground">
        Invalid task ID
      </div>
    )
  }

  return (
    <div className="p-4 space-y-3 max-w-5xl">
        <Button
          variant="ghost"
          size="sm"
          onClick={() => void navigate({ to: "/agents" })}
          className="gap-1 -ml-2 h-7 text-xs text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft className="size-3.5" />
          Tasks
        </Button>

        <TaskDetailCard jobId={jobId} compact={false} />
    </div>
  )
}
