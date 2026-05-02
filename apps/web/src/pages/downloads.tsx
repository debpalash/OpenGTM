import { useState, useEffect } from "react"
import { Download, Loader2, Trash2, RefreshCw, FileText, CheckCircle2, XCircle, Clock, HardDrive } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"

const API_BASE = ""

interface DownloadTask {
  id: string
  url: string
  title?: string
  status: string // pending, downloading, completed, failed
  progress?: number
  file_size?: string
  source?: string
  created_at?: string
  error?: string
}

const STATUS_CONFIG: Record<string, { icon: typeof Clock; color: string; label: string }> = {
  pending: { icon: Clock, color: "text-yellow-400", label: "Queued" },
  downloading: { icon: Loader2, color: "text-blue-400", label: "Downloading" },
  processing: { icon: Loader2, color: "text-purple-400", label: "Processing" },
  completed: { icon: CheckCircle2, color: "text-emerald-400", label: "Done" },
  failed: { icon: XCircle, color: "text-red-400", label: "Failed" },
}

export default function DownloadsPage() {
  const [tasks, setTasks] = useState<DownloadTask[]>([])
  const [loading, setLoading] = useState(true)

  const fetchTasks = () => {
    setLoading(true)
    fetch(`${API_BASE}/api/queue`)
      .then(r => r.json())
      .then(data => {
        const items = Array.isArray(data) ? data : data.tasks || data.queue || []
        setTasks(items)
      })
      .catch(() => setTasks([]))
      .finally(() => setLoading(false))
  }

  useEffect(() => {
    fetchTasks()
    const interval = setInterval(fetchTasks, 5000)
    return () => clearInterval(interval)
  }, [])

  const deleteTask = (id: string) => {
    fetch(`${API_BASE}/api/queue/${id}`, { method: "DELETE" })
      .then(() => setTasks(prev => prev.filter(t => t.id !== id)))
      .catch(() => {})
  }

  const retryTask = (id: string) => {
    fetch(`${API_BASE}/api/queue/${id}/retry`, { method: "POST" })
      .then(() => fetchTasks())
      .catch(() => {})
  }

  const activeCount = tasks.filter(t => t.status === "downloading" || t.status === "processing").length
  const completedCount = tasks.filter(t => t.status === "completed").length
  const failedCount = tasks.filter(t => t.status === "failed").length

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* Header */}
      <div className="p-4 border-b border-border">
        <div className="flex items-center justify-between">
          <div>
            <h2 className="text-sm font-semibold flex items-center gap-2">
              <Download size={16} className="text-primary" />
              Download Queue
            </h2>
            <p className="text-[11px] text-muted-foreground mt-0.5">
              {tasks.length} total · {activeCount} active · {completedCount} done · {failedCount} failed
            </p>
          </div>
          <Button variant="outline" size="sm" onClick={fetchTasks} className="h-7 text-xs">
            <RefreshCw size={12} className="mr-1.5" /> Refresh
          </Button>
        </div>

        {/* Progress summary */}
        {activeCount > 0 && (
          <div className="mt-3 h-1 bg-card rounded-full overflow-hidden">
            <div
              className="h-full bg-primary rounded-full transition-all animate-pulse"
              style={{ width: `${Math.min(100, (completedCount / Math.max(tasks.length, 1)) * 100)}%` }}
            />
          </div>
        )}
      </div>

      {/* Tasks */}
      <ScrollArea className="flex-1">
        <div className="p-4 space-y-2">
          {loading && tasks.length === 0 && (
            <div className="text-center py-20">
              <Loader2 className="mx-auto mb-3 animate-spin text-primary" size={32} />
              <p className="text-sm text-muted-foreground">Loading queue…</p>
            </div>
          )}

          {!loading && tasks.length === 0 && (
            <div className="text-center py-20 text-muted-foreground">
              <HardDrive className="mx-auto mb-3 opacity-20" size={48} />
              <p className="text-sm">No downloads in queue</p>
              <p className="text-xs mt-1 text-muted-foreground/60">Search for documents and click Download to add them here</p>
            </div>
          )}

          {tasks.map(task => {
            const config = STATUS_CONFIG[task.status] || STATUS_CONFIG.pending
            const Icon = config.icon
            return (
              <Card key={task.id} className="p-3 hover:border-primary/30 transition-all">
                <div className="flex items-center gap-3">
                  {/* Status icon */}
                  <div className={`shrink-0 ${config.color}`}>
                    <Icon size={18} className={task.status === "downloading" || task.status === "processing" ? "animate-spin" : ""} />
                  </div>

                  {/* Info */}
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2">
                      <h3 className="text-xs font-medium truncate">{task.title || task.url}</h3>
                      <Badge variant="outline" className={`text-[9px] ${config.color}`}>{config.label}</Badge>
                    </div>
                    <div className="flex items-center gap-2 mt-0.5 text-[10px] text-muted-foreground">
                      {task.source && <span>{task.source}</span>}
                      {task.file_size && <span>· {task.file_size}</span>}
                      {task.created_at && <span>· {new Date(task.created_at).toLocaleTimeString()}</span>}
                    </div>
                    {task.error && <p className="text-[10px] text-destructive mt-0.5">{task.error}</p>}

                    {/* Progress bar */}
                    {(task.status === "downloading") && task.progress !== undefined && (
                      <div className="mt-1.5 h-1 bg-card rounded-full overflow-hidden">
                        <div className="h-full bg-primary rounded-full transition-all" style={{ width: `${task.progress}%` }} />
                      </div>
                    )}
                  </div>

                  {/* Actions */}
                  <div className="flex gap-1 shrink-0">
                    {task.status === "completed" && (
                      <Button variant="outline" size="icon" className="h-7 w-7" onClick={() => window.open(`${API_BASE}/api/files/download/${task.id}`)}>
                        <FileText size={12} />
                      </Button>
                    )}
                    {task.status === "failed" && (
                      <Button variant="outline" size="icon" className="h-7 w-7" onClick={() => retryTask(task.id)}>
                        <RefreshCw size={12} />
                      </Button>
                    )}
                    <Button variant="ghost" size="icon" className="h-7 w-7 text-muted-foreground hover:text-destructive" onClick={() => deleteTask(task.id)}>
                      <Trash2 size={12} />
                    </Button>
                  </div>
                </div>
              </Card>
            )
          })}
        </div>
      </ScrollArea>
    </div>
  )
}
