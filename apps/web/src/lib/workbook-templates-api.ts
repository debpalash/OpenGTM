export type WorkbookTemplate = {
  id: string
  name: string
  description?: string
  columns?: unknown[]
}

async function readResponse(response: Response, fallback: string): Promise<unknown> {
  const payload: unknown = await response.json().catch(() => null)
  if (!response.ok) {
    const detail = payload && typeof payload === "object" && "detail" in payload ? payload.detail : null
    throw new Error(typeof detail === "string" ? detail : fallback)
  }
  return payload
}

export async function fetchWorkbookTemplates(category: string, signal?: AbortSignal): Promise<WorkbookTemplate[]> {
  const query = category ? `?${new URLSearchParams({ category })}` : ""
  const payload = await readResponse(await fetch(`/api/templates${query}`, { signal }), "Could not load templates. Try again.")
  if (!payload || typeof payload !== "object" || !("templates" in payload) || !Array.isArray(payload.templates)) {
    throw new Error("The server returned an invalid template list. Try again.")
  }
  return payload.templates.map((template: unknown) => {
    if (!template || typeof template !== "object" || !("id" in template) || typeof template.id !== "string" || !("name" in template) || typeof template.name !== "string") {
      throw new Error("The server returned an invalid template. Try again.")
    }
    return {
      id: template.id,
      name: template.name,
      description: "description" in template && typeof template.description === "string" ? template.description : undefined,
      columns: "columns" in template && Array.isArray(template.columns) ? template.columns : undefined,
    }
  })
}

export async function createWorkbookFromTemplate(id: string): Promise<{ id: string }> {
  const payload = await readResponse(await fetch(`/api/templates/${encodeURIComponent(id)}/create`, { method: "POST" }), "Could not create this workbook. Try again.")
  if (!payload || typeof payload !== "object" || !("id" in payload) || typeof payload.id !== "string" || !payload.id.trim()) {
    throw new Error("The server did not return a workbook ID. Check your workbooks before retrying.")
  }
  return { id: payload.id }
}
