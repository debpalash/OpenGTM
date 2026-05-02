import { useState } from "react"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Separator } from "@/components/ui/separator"
import { addLead } from "@/lib/api"

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  onAdded: () => void
}

function FormField({ label, name, required, type = "text" }: { label: string; name: string; required?: boolean; type?: string }) {
  return (
    <div className="space-y-0.5">
      <label className="text-[10px] text-zinc-500 uppercase tracking-wider font-medium">{label}{required && " *"}</label>
      <Input name={name} required={required} type={type} className="h-7 text-xs bg-zinc-900 border-zinc-700" />
    </div>
  )
}

export function AddLeadDialog({ open, onOpenChange, onAdded }: Props) {
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    setLoading(true)
    const fd = new FormData(e.currentTarget)
    const data = Object.fromEntries(fd)
    await addLead(data as Record<string, string>)
    setLoading(false)
    e.currentTarget.reset()
    onAdded()
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="bg-zinc-950 border-zinc-800 max-w-md">
        <DialogHeader>
          <DialogTitle className="text-sm">Add Lead</DialogTitle>
          <DialogDescription className="text-[11px] text-zinc-500">Add a new company to the lead database.</DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit}>
          <Card className="bg-zinc-900/50 border-zinc-800">
            <CardContent className="grid grid-cols-2 gap-2 p-3">
              <FormField label="Company" name="company" required />
              <FormField label="City" name="city" />
              <FormField label="Website" name="website" />
              <FormField label="Email" name="email" type="email" />
              <FormField label="Phone" name="phone" />
              <FormField label="Specialization" name="specialization" />
              <FormField label="Contact Person" name="contact_person" />
              <FormField label="LinkedIn URL" name="linkedin_url" />
            </CardContent>
          </Card>
          <div className="mt-2">
            <FormField label="Notes" name="notes" />
          </div>
          <Separator className="my-3" />
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" size="sm" className="h-7 text-xs" onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button type="submit" size="sm" className="h-7 text-xs" disabled={loading}>{loading ? "Adding…" : "Add Lead"}</Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}
