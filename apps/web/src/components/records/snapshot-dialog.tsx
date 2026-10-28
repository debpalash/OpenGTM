import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"

export function SnapshotDialog({ onClose, onRestored }: { onClose: () => void; onRestored: () => void }) {
  const [raw, setRaw] = useState(""); const [key] = useState(() => crypto.randomUUID())
  const [preview, setPreview] = useState<{ companies: number; people: number } | null>(null)
  const [error, setError] = useState(""); const [busy, setBusy] = useState(false)
  async function run(dry: boolean) {
    setBusy(true); setError("")
    try {
      // Preserve the file's number tokens exactly, including large custom IDs.
      const res = await fetch(`/api/record-snapshots/${dry ? "preview" : "restore"}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: `{"idempotency_key":${JSON.stringify(key)},"snapshot":${raw}}` })
      const output = await res.json(); if (!res.ok) throw new Error(output.detail || "Restore failed")
      if (dry) setPreview(output); else { onRestored(); onClose() }
    } catch (e) { setError(e instanceof Error ? e.message : "Could not restore snapshot") } finally { setBusy(false) }
  }
  return <Dialog open onOpenChange={open => { if (!open && !busy) onClose() }}><DialogContent className="sm:max-w-lg"><DialogHeader><DialogTitle>Restore an OpenGTM backup</DialogTitle><DialogDescription>Restore companies, people, employment history, accepted facts, observations, and source identities into a new, empty workspace. Record IDs are mapped to this workspace.</DialogDescription></DialogHeader>
    <label className="grid gap-2 text-xs font-medium">OpenGTM records JSON<Input type="file" accept=".json,application/json" disabled={busy} onChange={async e => { const file = e.target.files?.[0]; setPreview(null); setError(""); if (!file) return; if (file.size > 100 * 1024 * 1024) { setError("Use a backup up to 100 MB."); return } setRaw(await file.text()) }} /></label>
    {preview && <p className="rounded-md border bg-muted/30 p-3 text-sm">Ready to restore {preview.companies} companies and {preview.people} people, with their history and source identities.</p>}
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
    <div className="flex justify-end gap-2"><Button variant="outline" disabled={busy} onClick={onClose}>Cancel</Button><Button variant="outline" disabled={busy || !raw} onClick={() => void run(true)}>Validate backup</Button><Button disabled={busy || !preview} onClick={() => void run(false)}>Restore records</Button></div>
  </DialogContent></Dialog>
}
