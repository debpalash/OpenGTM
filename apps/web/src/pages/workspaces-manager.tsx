import { useState, useEffect } from "react"
import { toast } from "sonner"
import {
  Building2, Plus, Trash2, Check, LayoutDashboard, Users,
  ArrowRight, Loader2,
} from "lucide-react"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"

interface WorkspaceItem {
  id: string
  name: string
  slug: string
  description: string
  icon: string
  leads_count: number
  is_active: boolean
  created_at: number
}

export default function WorkspacesManagerPage() {
  const [workspaces, setWorkspaces] = useState<WorkspaceItem[]>([])
  const [, setActiveId] = useState("")
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState("")
  const [newDesc, setNewDesc] = useState("")
  const [creating, setCreating] = useState(false)
  const [dashboard, setDashboard] = useState<{ total_workspaces: number; total_leads: number } | null>(null)

  const fetchAll = async () => {
    try {
      const [wsRes, dashRes] = await Promise.all([
        fetch("/api/workspaces"),
        fetch("/api/workspaces/dashboard"),
      ])
      const wsData = await wsRes.json()
      const dashData = await dashRes.json()
      setWorkspaces(wsData.workspaces || [])
      setActiveId(wsData.active_id || "")
      setDashboard(dashData)
    } catch { /* ignore */ }
    setLoading(false)
  }

  useEffect(() => { fetchAll() }, [])

  const handleCreate = async () => {
    if (!newName.trim()) { toast.error("Enter workspace name"); return }
    setCreating(true)
    try {
      await fetch("/api/workspaces", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: newName, description: newDesc }),
      })
      toast.success(`Workspace "${newName}" created`)
      setNewName(""); setNewDesc(""); setShowCreate(false)
      fetchAll()
    } catch {
      toast.error("Failed to create workspace")
    }
    setCreating(false)
  }

  const switchTo = async (id: string) => {
    try {
      await fetch("/api/workspaces/switch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ workspace_id: id }),
      })
      toast.success("Workspace switched")
      fetchAll()
    } catch {
      toast.error("Failed to switch")
    }
  }

  const deleteWs = async (id: string) => {
    try {
      const res = await fetch(`/api/workspaces/${id}`, { method: "DELETE" })
      if (!res.ok) { toast.error("Cannot delete default workspace"); return }
      toast.success("Workspace deleted")
      fetchAll()
    } catch {
      toast.error("Failed to delete")
    }
  }

  return (
    <div className="flex flex-col gap-4 p-4 max-w-3xl">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold flex items-center gap-2">
            <Building2 className="size-4" />
            Workspaces
          </h2>
          <p className="text-xs text-muted-foreground mt-0.5">
            Isolated environments for each client. One workspace per account.
          </p>
        </div>
        <Button size="sm" onClick={() => setShowCreate(!showCreate)} className="h-7 text-xs gap-1">
          <Plus className="size-3" /> New Workspace
        </Button>
      </div>

      <Separator />

      {/* Agency Dashboard */}
      {dashboard && (
        <div className="flex items-center gap-6 text-xs">
          <div className="flex items-center gap-2">
            <LayoutDashboard className="size-3.5 text-muted-foreground" />
            <span className="font-medium text-foreground">{dashboard.total_workspaces}</span>
            <span className="text-muted-foreground">workspaces</span>
          </div>
          <div className="flex items-center gap-2">
            <Users className="size-3.5 text-muted-foreground" />
            <span className="font-medium text-foreground">{dashboard.total_leads}</span>
            <span className="text-muted-foreground">total leads</span>
          </div>
        </div>
      )}

      {/* Create Form */}
      {showCreate && (
        <Card>
          <CardContent className="pt-4 space-y-3">
            <div className="grid gap-3 md:grid-cols-2">
              <div className="space-y-1">
                <Label className="text-xs">Name</Label>
                <Input placeholder="e.g. Acme Corp" value={newName} onChange={e => setNewName(e.target.value)} className="text-xs" />
              </div>
              <div className="space-y-1">
                <Label className="text-xs">Description</Label>
                <Input placeholder="Client account" value={newDesc} onChange={e => setNewDesc(e.target.value)} className="text-xs" />
              </div>
            </div>
            <div className="flex gap-2">
              <Button size="sm" onClick={handleCreate} disabled={creating} className="h-7 text-xs gap-1">
                {creating ? <Loader2 className="size-3 animate-spin" /> : <Plus className="size-3" />}
                Create
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setShowCreate(false)} className="h-7 text-xs">Cancel</Button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Workspace List */}
      {loading ? (
        <div className="text-sm text-muted-foreground py-8 text-center">Loading...</div>
      ) : (
        <div className="space-y-2">
          {workspaces.map(ws => (
            <div
              key={ws.id}
              className={`flex items-center gap-3 px-3 py-2.5 rounded-lg border transition-colors ${
                ws.is_active ? "border-primary/30 bg-primary/5" : "hover:bg-muted/30"
              }`}
            >
              <span className="text-lg">{ws.icon}</span>
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium truncate">{ws.name}</span>
                  {ws.is_active && (
                    <span className="inline-flex items-center gap-1 text-[10px] text-primary bg-primary/10 px-1.5 py-0.5 rounded-full">
                      <Check className="size-2.5" /> Active
                    </span>
                  )}
                </div>
                <div className="flex items-center gap-3 mt-0.5 text-[11px] text-muted-foreground">
                  <span>{ws.leads_count} leads</span>
                  <span>{ws.slug}</span>
                </div>
              </div>
              <div className="flex items-center gap-1">
                {!ws.is_active && (
                  <Button size="sm" variant="outline" onClick={() => switchTo(ws.id)} className="h-6 text-[10px] gap-1">
                    <ArrowRight className="size-3" /> Switch
                  </Button>
                )}
                {ws.slug !== "main" && (
                  <Button size="sm" variant="ghost" onClick={() => deleteWs(ws.id)} className="h-6 w-6 p-0">
                    <Trash2 className="size-3 text-muted-foreground" />
                  </Button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
