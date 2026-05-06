/**
 * Workbooks List Page — create, list, and navigate to workbooks.
 */

import { useState } from "react"
import { useNavigate } from "react-router-dom"
import { useWorkbooks, useCreateWorkbook, useDeleteWorkbook } from "@/lib/workbook-hooks"
import { Plus, Table2, Trash2, Play, Pause, Clock, MoreHorizontal, FileSpreadsheet, Sparkles } from "lucide-react"
import { toast } from "sonner"

const STATUS_COLORS: Record<string, string> = {
  draft: "bg-zinc-500/20 text-zinc-400",
  running: "bg-emerald-500/20 text-emerald-400",
  paused: "bg-amber-500/20 text-amber-400",
  complete: "bg-blue-500/20 text-blue-400",
}

const STATUS_ICONS: Record<string, typeof Clock> = {
  draft: Clock,
  running: Play,
  paused: Pause,
  complete: Sparkles,
}

export default function WorkbooksPage() {
  const navigate = useNavigate()
  const { data, isLoading } = useWorkbooks()
  const createMutation = useCreateWorkbook()
  const deleteMutation = useDeleteWorkbook()
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState("")

  const handleCreate = async () => {
    if (!newName.trim()) return
    try {
      const wb = await createMutation.mutateAsync({
        name: newName.trim(),
        description: "",
        columns_config: [
          { id: "company", name: "Company", type: "lead_field", width: 200, lead_field: "company" },
          { id: "website", name: "Website", type: "lead_field", width: 200, lead_field: "website" },
          { id: "email", name: "Email", type: "lead_field", width: 220, lead_field: "email" },
          { id: "phone", name: "Phone", type: "lead_field", width: 160, lead_field: "phone" },
          { id: "city", name: "City", type: "lead_field", width: 140, lead_field: "city" },
          { id: "score", name: "Score", type: "lead_field", width: 80, lead_field: "score" },
        ],
      })
      toast.success("Workbook created")
      setShowCreate(false)
      setNewName("")
      navigate(`/workbooks/${wb.id}`)
    } catch {
      toast.error("Failed to create workbook")
    }
  }

  const handleDelete = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    if (!confirm("Delete this workbook and all its data?")) return
    try {
      await deleteMutation.mutateAsync(id)
      toast.success("Workbook deleted")
    } catch {
      toast.error("Failed to delete")
    }
  }

  const workbooks = data?.workbooks || []

  return (
    <div className="p-6 max-w-5xl mx-auto space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">Workbooks</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Programmable spreadsheets with built-in enrichment
          </p>
        </div>
        <button
          onClick={() => setShowCreate(true)}
          className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-primary text-primary-foreground text-sm font-medium hover:bg-primary/90 transition-colors"
        >
          <Plus className="size-4" />
          New Workbook
        </button>
      </div>

      {/* Create Dialog */}
      {showCreate && (
        <div className="rounded-xl border bg-card p-4 space-y-3 shadow-lg animate-in fade-in slide-in-from-top-2 duration-200">
          <input
            autoFocus
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && handleCreate()}
            placeholder="Workbook name..."
            className="w-full px-3 py-2 rounded-lg border bg-background text-sm focus:outline-none focus:ring-2 focus:ring-primary/50"
          />
          <div className="flex gap-2 justify-end">
            <button
              onClick={() => { setShowCreate(false); setNewName("") }}
              className="px-3 py-1.5 text-sm rounded-md hover:bg-muted transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleCreate}
              disabled={!newName.trim() || createMutation.isPending}
              className="px-3 py-1.5 text-sm rounded-md bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-50 transition-colors"
            >
              {createMutation.isPending ? "Creating..." : "Create"}
            </button>
          </div>
        </div>
      )}

      {/* Loading */}
      {isLoading && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {[1, 2, 3].map(i => (
            <div key={i} className="rounded-xl border bg-card p-5 animate-pulse">
              <div className="h-5 w-32 bg-muted rounded mb-3" />
              <div className="h-3 w-48 bg-muted rounded mb-4" />
              <div className="h-8 w-full bg-muted rounded" />
            </div>
          ))}
        </div>
      )}

      {/* Empty State */}
      {!isLoading && workbooks.length === 0 && !showCreate && (
        <div className="rounded-xl border-2 border-dashed bg-card/50 p-16 text-center">
          <FileSpreadsheet className="size-12 mx-auto text-muted-foreground/50 mb-4" />
          <h3 className="text-lg font-medium mb-2">No workbooks yet</h3>
          <p className="text-sm text-muted-foreground mb-6 max-w-md mx-auto">
            Create your first workbook to start enriching leads with waterfall providers,
            AI formulas, and real-time data.
          </p>
          <button
            onClick={() => setShowCreate(true)}
            className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-primary text-primary-foreground text-sm font-medium hover:bg-primary/90 transition-colors"
          >
            <Plus className="size-4" />
            Create Workbook
          </button>
        </div>
      )}

      {/* Workbook Grid */}
      {workbooks.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {workbooks.map(wb => {
            const StatusIcon = STATUS_ICONS[wb.status] || Clock
            const enrichmentCols = wb.columns_config.filter(c => c.type === "enrichment" || c.type === "waterfall" || c.type === "ai_formula").length
            const progress = wb.total_rows > 0 ? Math.round((wb.completed_rows / wb.total_rows) * 100) : 0

            return (
              <div
                key={wb.id}
                onClick={() => navigate(`/workbooks/${wb.id}`)}
                className="group rounded-xl border bg-card p-5 cursor-pointer hover:border-primary/50 hover:shadow-md transition-all duration-200"
              >
                {/* Header */}
                <div className="flex items-start justify-between mb-3">
                  <div className="flex items-center gap-2.5">
                    <div className="p-1.5 rounded-lg bg-primary/10">
                      <Table2 className="size-4 text-primary" />
                    </div>
                    <h3 className="font-semibold text-sm truncate max-w-[180px]">{wb.name}</h3>
                  </div>
                  <button
                    onClick={(e) => handleDelete(wb.id, e)}
                    className="opacity-0 group-hover:opacity-100 p-1 rounded hover:bg-destructive/10 hover:text-destructive transition-all"
                  >
                    <Trash2 className="size-3.5" />
                  </button>
                </div>

                {/* Stats Row */}
                <div className="flex items-center gap-3 text-xs text-muted-foreground mb-3">
                  <span>{wb.total_rows} rows</span>
                  <span>·</span>
                  <span>{wb.columns_config.length} cols</span>
                  {enrichmentCols > 0 && (
                    <>
                      <span>·</span>
                      <span className="text-primary">{enrichmentCols} enrichment</span>
                    </>
                  )}
                </div>

                {/* Status + Progress */}
                <div className="flex items-center justify-between">
                  <span className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-xs font-medium ${STATUS_COLORS[wb.status] || STATUS_COLORS.draft}`}>
                    <StatusIcon className="size-3" />
                    {wb.status}
                  </span>
                  {wb.total_rows > 0 && (
                    <div className="flex items-center gap-2">
                      <div className="w-16 h-1.5 rounded-full bg-muted overflow-hidden">
                        <div
                          className="h-full rounded-full bg-primary transition-all duration-500"
                          style={{ width: `${progress}%` }}
                        />
                      </div>
                      <span className="text-xs text-muted-foreground tabular-nums">{progress}%</span>
                    </div>
                  )}
                </div>

                {/* Last Updated */}
                {wb.updated_at && (
                  <p className="text-[10px] text-muted-foreground mt-2">
                    Updated {new Date(wb.updated_at).toLocaleDateString()}
                  </p>
                )}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
