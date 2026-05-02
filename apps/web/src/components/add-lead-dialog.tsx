import { useState } from "react"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Separator } from "@/components/ui/separator"
import { addLead } from "@/lib/api"

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  onAdded: () => void
}

function FormField({ label, name, required, type = "text", span }: {
  label: string; name: string; required?: boolean; type?: string; span?: boolean
}) {
  return (
    <div className={`space-y-1 ${span ? "col-span-2" : ""}`}>
      <label className="text-[10px] text-muted-foreground/50 uppercase tracking-wider font-semibold">
        {label}{required && <span className="text-primary ml-0.5">*</span>}
      </label>
      <Input
        name={name}
        required={required}
        type={type}
        className="h-8 text-[12px] bg-card/50 border-border/40 focus:border-primary/40 placeholder:text-muted-foreground/20"
      />
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
      <DialogContent className="bg-background border-border max-w-md p-0 gap-0">
        <DialogHeader className="px-5 pt-5 pb-3">
          <DialogTitle className="text-sm font-bold text-foreground">Add Lead</DialogTitle>
          <DialogDescription className="text-[11px] text-muted-foreground/60">
            Add a new company to the pipeline.
          </DialogDescription>
        </DialogHeader>

        <Separator />

        <form onSubmit={handleSubmit} className="px-5 py-4 space-y-4">
          {/* Primary fields */}
          <div>
            <div className="text-[10px] text-muted-foreground/40 uppercase tracking-widest font-semibold mb-2.5">Identity</div>
            <div className="grid grid-cols-2 gap-2.5">
              <FormField label="Company" name="company" required />
              <FormField label="City" name="city" />
            </div>
          </div>

          {/* Contact */}
          <div>
            <div className="text-[10px] text-muted-foreground/40 uppercase tracking-widest font-semibold mb-2.5">Contact</div>
            <div className="grid grid-cols-2 gap-2.5">
              <FormField label="Website" name="website" />
              <FormField label="Email" name="email" type="email" />
              <FormField label="Phone" name="phone" />
              <FormField label="LinkedIn URL" name="linkedin_url" />
            </div>
          </div>

          {/* Details */}
          <div>
            <div className="text-[10px] text-muted-foreground/40 uppercase tracking-widest font-semibold mb-2.5">Details</div>
            <div className="grid grid-cols-2 gap-2.5">
              <FormField label="Specialization" name="specialization" />
              <FormField label="Contact Person" name="contact_person" />
              <FormField label="Notes" name="notes" span />
            </div>
          </div>

          <Separator />

          {/* Actions */}
          <div className="flex justify-end gap-2 pt-1">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-8 text-[11px] text-muted-foreground"
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              size="sm"
              className="h-8 text-[11px] px-5"
              disabled={loading}
            >
              {loading ? "Adding…" : "Add Lead"}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}
