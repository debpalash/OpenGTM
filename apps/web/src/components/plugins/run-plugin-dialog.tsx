// "Run plugin" dialog: a form generated from the plugin's `inputs` JSON Schema.

import { useMemo, useRef, useState, type FormEvent } from "react"
import { Globe, KeyRound, Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect } from "@/components/ui/native-select"
import { Textarea } from "@/components/ui/textarea"
import { ApiError } from "@/lib/api"
import { apiErrorMessage } from "@/lib/automation-hooks"
import {
  buildInputs, enumOptionValue, FREEFORM_FIELD, initialValues, schemaToForm, type FormField, type FormValues,
} from "@/lib/json-schema-form"
import { isPlatformUnavailable, type PluginInfo, type PluginRun } from "@/lib/plugin-api"
import { pluginTitle } from "@/lib/plugin-catalog"
import { useCreatePluginRun } from "@/lib/plugin-hooks"

function runErrorMessage(error: unknown): string {
  if (isPlatformUnavailable(error)) return error.message
  if (error instanceof ApiError) {
    if (error.status === 403) return "Your workspace role cannot start plugin runs. Ask an editor or admin."
    if (error.status === 404) return "This plugin is no longer installed on the server. Reload the catalog."
    if (error.status === 422 || error.status === 400) return error.detail
  }
  return apiErrorMessage(error)
}

function FieldControl({ field, id, value, invalid, describedBy, onChange }: {
  field: FormField; id: string; value: string; invalid: boolean; describedBy?: string; onChange: (value: string) => void
}) {
  const common = { id, "aria-invalid": invalid || undefined, "aria-describedby": describedBy, "aria-required": field.required || undefined, name: field.name }
  switch (field.kind) {
    case "boolean":
      if (field.required) {
        return <div className="flex items-center gap-2">
          <Checkbox {...common} checked={value === "true"} onCheckedChange={checked => onChange(checked ? "true" : "false")} />
          <span className="text-xs text-muted-foreground">{value === "true" ? "Yes" : "No"}</span>
        </div>
      }
      return <NativeSelect {...common} value={value} onChange={event => onChange(event.target.value)} className="w-full">
        <option value="">Not set</option><option value="true">Yes</option><option value="false">No</option>
      </NativeSelect>
    case "enum":
      return <NativeSelect {...common} value={value} onChange={event => onChange(event.target.value)} className="w-full">
        <option value="">{field.required ? "Choose…" : "Not set"}</option>
        {field.options?.map(option => <option key={enumOptionValue(option)} value={enumOptionValue(option)}>{String(option)}</option>)}
      </NativeSelect>
    case "json":
      return <Textarea {...common} value={value} onChange={event => onChange(event.target.value)} rows={4} spellCheck={false}
        className="font-mono text-xs" placeholder={field.jsonType === "array" ? "[]" : field.jsonType === "object" ? "{}" : "JSON value"} />
    case "number":
    case "integer":
      return <Input {...common} value={value} onChange={event => onChange(event.target.value)} inputMode={field.kind === "integer" ? "numeric" : "decimal"}
        placeholder={field.placeholder ?? (field.minimum !== undefined || field.maximum !== undefined ? [field.minimum ?? "", field.maximum ?? ""].join(" – ") : undefined)} autoComplete="off" />
    default:
      return <Input {...common} value={value} onChange={event => onChange(event.target.value)} placeholder={field.placeholder}
        inputMode={field.format === "email" ? "email" : field.format === "uri" ? "url" : undefined} autoComplete="off" spellCheck={false} />
  }
}

function RunForm({ plugin, onClose, onCreated }: { plugin: PluginInfo; onClose: () => void; onCreated: (run: PluginRun) => void }) {
  const form = useMemo(() => schemaToForm(plugin.inputs), [plugin.inputs])
  const [values, setValues] = useState<FormValues>(() => initialValues(form))
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [formError, setFormError] = useState("")
  const create = useCreatePluginRun()
  const formRef = useRef<HTMLFormElement>(null)
  const idFor = (name: string) => `plugin-input-${plugin.name}-${name}`

  const fields: FormField[] = form.freeform
    ? [{ name: FREEFORM_FIELD, label: "Inputs (JSON object)", kind: "json", jsonType: "object", required: false,
      description: "This plugin declares no input fields; any JSON object is passed through." }, ...form.fields]
    : form.fields

  function submit(event: FormEvent) {
    event.preventDefault()
    if (create.isPending) return
    const result = buildInputs(form, values)
    setErrors(result.errors)
    setFormError("")
    const firstInvalid = fields.find(field => result.errors[field.name])
    if (firstInvalid) {
      formRef.current?.querySelector<HTMLElement>(`#${CSS.escape(idFor(firstInvalid.name))}`)?.focus()
      return
    }
    create.mutate({ plugin: plugin.name, inputs: result.inputs }, {
      onSuccess: run => { onCreated(run); onClose() },
      onError: error => setFormError(runErrorMessage(error)),
    })
  }

  return (
    <form ref={formRef} noValidate onSubmit={submit} className="grid gap-4" aria-label={`Inputs for ${pluginTitle(plugin)}`}>
      {(plugin.network.length > 0 || plugin.secrets.length > 0) && <div className="grid gap-1 rounded-md bg-muted/60 px-3 py-2 text-xs text-muted-foreground">
        {plugin.network.length > 0 && <p className="flex items-start gap-1.5"><Globe aria-hidden="true" className="mt-0.5 size-3 shrink-0" />
          <span>May fetch <span className="font-mono text-foreground/80">{plugin.network.join(", ")}</span></span></p>}
        {plugin.secrets.length > 0 && <p className="flex items-start gap-1.5"><KeyRound aria-hidden="true" className="mt-0.5 size-3 shrink-0" />
          <span>Reads workspace secrets <span className="font-mono text-foreground/80">{plugin.secrets.join(", ")}</span></span></p>}
      </div>}
      {fields.length === 0
        ? <p className="text-xs text-muted-foreground">This plugin takes no inputs.</p>
        : <div className="grid max-h-[55vh] gap-3 overflow-y-auto pr-1">
          {fields.map(field => {
            const id = idFor(field.name)
            const helpId = field.description ? `${id}-help` : undefined
            const errorId = errors[field.name] ? `${id}-error` : undefined
            return <div key={field.name} className="grid gap-1.5">
              <label htmlFor={id} className="flex items-baseline gap-1 text-xs font-medium">
                {field.label}
                {field.required ? <span aria-hidden="true" className="text-destructive">*</span> : <span className="font-normal text-muted-foreground">(optional)</span>}
                {field.required && <span className="sr-only">(required)</span>}
                {field.label.toLowerCase().replaceAll(" ", "_") !== field.name.toLowerCase() && field.name !== FREEFORM_FIELD && <code className="ml-auto font-mono text-[10px] font-normal text-muted-foreground">{field.name}</code>}
              </label>
              <FieldControl field={field} id={id} value={values[field.name] ?? ""} invalid={!!errors[field.name]}
                describedBy={[errorId, helpId].filter(Boolean).join(" ") || undefined}
                onChange={value => {
                  setValues(current => ({ ...current, [field.name]: value }))
                  if (errors[field.name]) setErrors(({ [field.name]: _cleared, ...rest }) => rest)
                }} />
              {errorId && <p id={errorId} className="text-xs text-destructive">{errors[field.name]}</p>}
              {helpId && <p id={helpId} className="text-[11px] text-muted-foreground">{field.description}</p>}
            </div>
          })}
        </div>}
      {formError && <p role="alert" className="rounded-md bg-destructive/10 px-3 py-2 text-xs text-destructive">{formError}</p>}
      <DialogFooter className="-mx-5 -mb-5">
        <Button type="button" variant="outline" onClick={onClose} disabled={create.isPending}>Cancel</Button>
        <Button type="submit" disabled={create.isPending}>
          {create.isPending && <Loader2 aria-hidden="true" className="motion-safe:animate-spin" />}
          {create.isPending ? "Starting…" : "Start run"}
        </Button>
      </DialogFooter>
    </form>
  )
}

export function RunPluginDialog({ plugin, onOpenChange, onCreated }: {
  plugin: PluginInfo | null; onOpenChange: (open: boolean) => void; onCreated: (run: PluginRun) => void
}) {
  return (
    <Dialog open={!!plugin} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        {plugin && <>
          <DialogHeader>
            <DialogTitle>Run {pluginTitle(plugin)}</DialogTitle>
            <DialogDescription>
              Queues a {plugin.kind} run on the plugin host{plugin.version ? ` (v${plugin.version})` : ""}. Results and evidence appear under Runs.
            </DialogDescription>
          </DialogHeader>
          <RunForm key={plugin.name} plugin={plugin} onClose={() => onOpenChange(false)} onCreated={onCreated} />
        </>}
      </DialogContent>
    </Dialog>
  )
}
