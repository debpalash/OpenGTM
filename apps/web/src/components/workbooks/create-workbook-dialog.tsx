import { useRef, useState } from "react"
import { useNavigate } from "react-router-dom"
import { Plus } from "lucide-react"
import { toast } from "sonner"
import { Button, Dialog, Input } from "@/design-system/primitives"
import { useCreateWorkbook } from "@/lib/workbook-hooks"
import { NativeSelect } from "@/components/ui/native-select"

const DEFAULT_COLUMNS = [
  { id: "company", name: "Company", width: 200 },
  { id: "website", name: "Website", width: 200 },
  { id: "email", name: "Email", width: 220 },
  { id: "phone", name: "Phone", width: 160 },
  { id: "city", name: "City", width: 140 },
  { id: "score", name: "Score", width: 80 },
]

export function CreateWorkbookDialog() {
  const navigate = useNavigate()
  const mutation = useCreateWorkbook()
  const submitting = useRef(false)
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [source, setSource] = useState("empty")
  const [tier, setTier] = useState("all")
  const [limit, setLimit] = useState("500")
  const [error, setError] = useState("")

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    if (submitting.current || !name.trim()) return
    submitting.current = true
    setError("")
    try {
      const workbook = await mutation.mutateAsync({
        name: name.trim(), description: "", source: source === "leads" ? "leads_filter" : "empty",
        filter_criteria: source === "leads" && tier !== "all" ? { score_tier: tier } : undefined,
        max_rows: source === "leads" ? Number(limit) : undefined,
        columns_config: DEFAULT_COLUMNS.map(column => ({ ...column, type: "lead_field", lead_field: column.id })),
      })
      toast.success("Workbook created")
      setOpen(false)
      navigate(`/workbooks/${encodeURIComponent(workbook.id)}`)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not create workbook. Try again.")
    } finally { submitting.current = false }
  }

  return <Dialog.Root open={open} onOpenChange={(next, details) => {
    if (submitting.current) { details.cancel(); return }
    setOpen(next)
    if (next) setError("")
  }}>
    <Dialog.Trigger render={<Button variant="solid" startIcon={<Plus className="size-4" />} />}>New workbook</Dialog.Trigger>
    <Dialog.Popup className="gtm-workbook-dialog">
      <Dialog.Header><Dialog.Title>New workbook</Dialog.Title><Dialog.Description>Start empty or copy a snapshot of your existing leads.</Dialog.Description></Dialog.Header>
      <form onSubmit={submit}>
        <Dialog.Body className="space-y-4">
          {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
          <label className="grid gap-2 text-sm">Workbook name<Input value={name} onChange={event => setName(event.target.value)} maxLength={255} required disabled={mutation.isPending} /></label>
          <fieldset className="space-y-2" disabled={mutation.isPending}>
            <legend className="mb-2 text-sm font-medium">Starting rows</legend>
            <label className="flex items-center gap-2 text-sm"><input type="radio" name="source" value="empty" checked={source === "empty"} onChange={() => setSource("empty")} />Start empty</label>
            <label className="flex items-center gap-2 text-sm"><input type="radio" name="source" value="leads" checked={source === "leads"} onChange={() => setSource("leads")} />From my leads</label>
          </fieldset>
          {source === "leads" && <div className="grid grid-cols-2 gap-3">
            <label className="grid gap-2 text-sm">Score tier<NativeSelect aria-label="Score tier" className="w-full" value={tier} onChange={event => setTier(event.target.value)} disabled={mutation.isPending}>
              <option value="all">All tiers</option><option value="hot">Hot</option><option value="warm">Warm</option><option value="cold">Cold</option>
            </NativeSelect></label>
            <label className="grid gap-2 text-sm">Maximum rows<Input type="number" min={1} max={5000} step={1} required value={limit} onChange={event => setLimit(event.target.value)} disabled={mutation.isPending} aria-describedby="row-limit-help" /></label>
            <p id="row-limit-help" className="col-span-2 text-xs text-muted-foreground">Up to 5,000 rows, ordered by score. Original leads are unchanged.</p>
          </div>}
        </Dialog.Body>
        <Dialog.Footer><Dialog.Close render={<Button disabled={mutation.isPending} />}>Cancel</Dialog.Close><Button type="submit" variant="solid" disabled={!name.trim()} loading={mutation.isPending}>Create workbook</Button></Dialog.Footer>
      </form>
    </Dialog.Popup>
  </Dialog.Root>
}
