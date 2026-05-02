import { useCallback, useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { fetchWorkspaces, createWorkspace, deleteWorkspace, type Workspace } from "@/lib/api"

interface Props {
  activeId: string
  onSelect: (id: string) => void
}

export function WorkspacePanel({ activeId, onSelect }: Props) {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState("")

  const load = useCallback(async () => {
    setWorkspaces(await fetchWorkspaces())
  }, [])

  useEffect(() => { load() }, [load])

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!newName.trim()) return
    const result = await createWorkspace(newName.trim())
    setNewName("")
    setCreating(false)
    load()
    onSelect(result.id)
  }

  const handleDelete = async (id: string, name: string) => {
    if (!confirm(`Delete workspace "${name}" and all its leads?`)) return
    await deleteWorkspace(id)
    if (activeId === id) onSelect("")
    load()
  }

  const totalLeads = workspaces.reduce((sum, w) => sum + w.active_lead_count, 0)

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <div className="flex items-center justify-between px-3 h-9 border-b border-border shrink-0">
        <span className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">Workspaces</span>
        <Button
          variant="ghost"
          size="sm"
          className="h-5 w-5 p-0 text-[10px]"
          onClick={() => setCreating(!creating)}
        >
          +
        </Button>
      </div>

      {/* Create form */}
      {creating && (
        <form onSubmit={handleCreate} className="px-3 py-2 border-b border-border/50">
          <Input
            autoFocus
            placeholder="Workspace name…"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            className="h-6 text-[11px] mb-1"
          />
          <div className="flex gap-1">
            <Button type="submit" size="sm" className="h-5 text-[10px] flex-1" disabled={!newName.trim()}>Create</Button>
            <Button type="button" variant="ghost" size="sm" className="h-5 text-[10px]" onClick={() => setCreating(false)}>✕</Button>
          </div>
        </form>
      )}

      {/* Workspace list */}
      <ScrollArea className="flex-1">
        <div className="p-1.5 space-y-0.5">
          {/* All workspaces */}
          <button
            className={`w-full flex items-center justify-between px-2 py-1.5 rounded-md text-left text-[11px] transition-colors ${
              activeId === "" ? "bg-accent text-accent-foreground" : "hover:bg-accent/40 text-muted-foreground"
            }`}
            onClick={() => onSelect("")}
          >
            <span className="font-medium">All Leads</span>
            <Badge variant="secondary" className="text-[9px] h-4 px-1.5">{totalLeads}</Badge>
          </button>

          {workspaces.length > 0 && <Separator className="my-1" />}

          {workspaces.map((ws) => (
            <div
              key={ws.id}
              className={`group flex items-center justify-between px-2 py-1.5 rounded-md transition-colors cursor-pointer ${
                activeId === ws.id ? "bg-accent text-accent-foreground" : "hover:bg-accent/40 text-muted-foreground"
              }`}
              onClick={() => onSelect(ws.id)}
            >
              <div className="min-w-0">
                <div className="text-[11px] font-medium truncate">{ws.name}</div>
                <div className="text-[9px] text-muted-foreground/60">{ws.job_count} jobs</div>
              </div>
              <div className="flex items-center gap-1">
                <Badge variant="secondary" className="text-[9px] h-4 px-1.5">{ws.active_lead_count}</Badge>
                <button
                  className="opacity-0 group-hover:opacity-100 text-[9px] text-destructive h-4 w-4 flex items-center justify-center"
                  onClick={(e) => { e.stopPropagation(); handleDelete(ws.id, ws.name) }}
                >
                  ✕
                </button>
              </div>
            </div>
          ))}
        </div>
      </ScrollArea>
    </div>
  )
}
