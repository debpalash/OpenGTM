import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { recordAPI } from "@/lib/records"
import { getActiveWorkspace } from "@/lib/auth"

export function EnrollPeopleDialog({ ids, onClose, onEnrolled }: { ids: string[]; onClose: () => void; onEnrolled: () => void }) {
  const [sequence, setSequence] = useState(""); const [consent, setConsent] = useState("")
  const [busy, setBusy] = useState(false); const [error, setError] = useState("")
  const sequences = useQuery({ queryKey: ["record-sequences", getActiveWorkspace()], queryFn: ({ signal }) => recordAPI<{ id: string; name: string }[]>("outreach/sequences", undefined, "GET", signal) })
  async function enroll() { setBusy(true); setError(""); try { const out = await recordAPI<{ enrolled: number; skipped: { reason: string }[] }>("records/person/enroll", { ids, sequence_id: sequence, consent_source: consent }); if (!out.enrolled && out.skipped.length) { setError(out.skipped.map(s => s.reason).join(", ")); return } onEnrolled() } catch (e) { setError(e instanceof Error ? e.message : "Could not enroll people") } finally { setBusy(false) } }
  return <Dialog open onOpenChange={open => { if (!open && !busy) onClose() }}><DialogContent><DialogHeader><DialogTitle>Enroll {ids.length} people</DialogTitle><DialogDescription>Use current email addresses and retain each person’s identity. Suppressed recipients and existing enrollments are skipped.</DialogDescription></DialogHeader>
    <label className="grid gap-1 text-xs font-medium">Sequence<select aria-label="Sequence" className="h-9 rounded-md border bg-background px-3 text-sm" value={sequence} onChange={e => setSequence(e.target.value)}><option value="">Choose a sequence</option>{sequences.data?.map(s => <option value={s.id} key={s.id}>{s.name}</option>)}</select></label>
    {sequences.isError && <p role="alert" className="text-xs text-destructive">Could not load sequences.</p>}
    <label className="grid gap-1 text-xs font-medium">Consent source<Input value={consent} maxLength={120} placeholder="For example: opted in at our event" onChange={e => setConsent(e.target.value)} /></label>
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
    <div className="flex justify-end gap-2"><Button variant="outline" disabled={busy} onClick={onClose}>Cancel</Button><Button disabled={busy || !sequence || !consent.trim()} onClick={() => void enroll()}>Enroll people</Button></div>
  </DialogContent></Dialog>
}
