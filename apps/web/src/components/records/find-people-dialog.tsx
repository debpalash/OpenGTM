import { useState } from "react"
import { useNavigate } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { recordAPI } from "@/lib/records"

export function FindPeopleDialog({ ids, onClose }: { ids: string[]; onClose: () => void }) {
  const navigate = useNavigate(); const [titles, setTitles] = useState("CEO, VP Sales, Head of Engineering")
  const [maxPeople, setMaxPeople] = useState(5); const [budget, setBudget] = useState(20)
  const [busy, setBusy] = useState(false); const [error, setError] = useState("")
  async function find() {
    setBusy(true); setError("")
    try {
      const out = await recordAPI<{ workbook_id: string }>("records/company/find-people", { ids, titles: titles.split(",").map(t => t.trim()).filter(Boolean), max_per_company: maxPeople, max_searches: budget })
      await recordAPI(`workbooks/${out.workbook_id}/run`, {})
      navigate(`/workbooks/${out.workbook_id}`)
    } catch (e) { setError(e instanceof Error ? e.message : "Could not start discovery") } finally { setBusy(false) }
  }
  return <Dialog open onOpenChange={open => { if (!open && !busy) onClose() }}><DialogContent><DialogHeader><DialogTitle>Find people at {ids.length} companies</DialogTitle><DialogDescription>Find matching roles and add independent people linked to your existing company records. Follow progress in the discovery workbook.</DialogDescription></DialogHeader>
    <label className="grid gap-1 text-xs font-medium">Job titles, separated by commas<Input value={titles} onChange={e => setTitles(e.target.value)} /></label>
    <label className="grid gap-1 text-xs font-medium">Maximum people per company<Input type="number" min={1} max={100} value={maxPeople} onChange={e => setMaxPeople(Number(e.target.value))} /></label>
    <label className="grid gap-1 text-xs font-medium">Search budget<Input type="number" min={1} max={1000} value={budget} onChange={e => setBudget(Number(e.target.value))} /><span className="font-normal text-muted-foreground">Maximum searches for this run. A larger budget can cover more companies and roles.</span></label>
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}<div className="flex justify-end gap-2"><Button variant="outline" disabled={busy} onClick={onClose}>Cancel</Button><Button disabled={busy || ids.length > 100 || maxPeople < 1 || budget < 1} onClick={() => void find()}>Find people</Button></div>
  </DialogContent></Dialog>
}
