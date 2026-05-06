import { useParams, useNavigate } from "react-router-dom"
import { ArrowLeft } from "lucide-react"
import { Button } from "@/components/ui/button"
import { TaskDetailCard } from "@/components/task-detail-card"

export default function TaskDetailPage() {
  const { jobId } = useParams<{ jobId: string }>()
  const navigate = useNavigate()

  if (!jobId) {
    return (
      <div className="flex items-center justify-center h-full text-muted-foreground">
        Invalid task ID
      </div>
    )
  }

  return (
    <div className="p-6 space-y-4">
        <Button
          variant="ghost"
          size="sm"
          onClick={() => navigate("/agents")}
          className="gap-1 -ml-2 text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft className="size-4" />
          Back to Tasks
        </Button>

        <TaskDetailCard jobId={jobId} compact={false} />
    </div>
  )
}
