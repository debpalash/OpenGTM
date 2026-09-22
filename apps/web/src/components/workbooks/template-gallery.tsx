import { useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useNavigate } from "react-router-dom"
import { toast } from "sonner"
import { Button } from "@/design-system/primitives"
import { fetchWorkbookTemplates, createWorkbookFromTemplate } from "@/lib/workbook-templates-api"
import { workbookKeys } from "@/lib/workbook-hooks"

export function TemplateGallery() {
  const [category, setCategory] = useState("")
  const query = useQuery({ queryKey: ["workbook-templates", category], queryFn: ({ signal }) => fetchWorkbookTemplates(category, signal) })
  const client = useQueryClient()
  const navigate = useNavigate()
  const busy = useRef(false)
  const create = useMutation({ mutationFn: createWorkbookFromTemplate })

  async function useTemplate(id: string) {
    if (busy.current) return
    busy.current = true
    try {
      const workbook = await create.mutateAsync(id)
      await client.invalidateQueries({ queryKey: workbookKeys.list() })
      toast.success("Workbook created from template")
      navigate(`/workbooks/${encodeURIComponent(workbook.id)}`)
    } catch { /* The mutation error is rendered in the gallery. */ }
    finally { busy.current = false }
  }

  return <section className="rounded-md border p-4" aria-label="Workbook templates">
    <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
      <h2 className="text-sm font-medium">Start from a template</h2>
      <label className="flex items-center gap-2 text-xs text-muted-foreground">Category<select aria-label="Category" className="gtm-select" value={category} onChange={event => setCategory(event.target.value)} disabled={create.isPending}>
        <option value="">All categories</option>{["sales", "recruiting", "research", "agency", "signals"].map(value => <option key={value} value={value}>{value}</option>)}
      </select></label>
    </div>
    {query.isPending && <p role="status" className="text-sm text-muted-foreground">Loading templates…</p>}
    {query.isError && <div role="alert" className="space-y-2"><p>{query.error.message}</p><Button onClick={() => void query.refetch()}>Retry templates</Button></div>}
    {create.isError && <p role="alert" className="mb-3 text-sm text-destructive">{create.error.message}</p>}
    {!query.isPending && !query.isError && !query.data?.length && <p className="text-sm text-muted-foreground">No templates in this category.</p>}
    <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">{query.data?.map(template => <button key={template.id} type="button" onClick={() => void useTemplate(template.id)} disabled={create.isPending}
      aria-label={`Use ${template.name}`} aria-busy={create.isPending && create.variables === template.id}
      className="rounded-md border p-3 text-left hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring disabled:opacity-50">
      <span className="block text-sm font-medium">{template.name}</span>
      {template.description && <span className="mt-1 block text-xs text-muted-foreground">{template.description}</span>}
      <span className="mt-2 block text-xs text-muted-foreground">{create.isPending && create.variables === template.id ? "Creating…" : `${template.columns?.length ?? 0} columns`}</span>
    </button>)}</div>
  </section>
}
