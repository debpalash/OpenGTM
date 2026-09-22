import { Button, Dialog } from "@/design-system/primitives"
import type { ResearchEvidence } from "@/lib/workbook-api"
import { citationHref } from "@/lib/research-evidence"

export function ResearchEvidenceInspector({ evidence }: { evidence: ResearchEvidence }) {
  return <Dialog.Root>
    <Dialog.Trigger render={<Button aria-label="Inspect research evidence" className="shrink-0" />}>
      {evidence.citations.length} sources
    </Dialog.Trigger>
    <Dialog.Popup size="lg" style={{ width: "min(640px, calc(100vw - 32px))", maxHeight: "calc(100dvh - 32px)" }}>
      <Dialog.Header>
        <Dialog.Title>Research evidence</Dialog.Title>
        <Dialog.Description>Sources supporting this research, not independent verification of every claim.</Dialog.Description>
      </Dialog.Header>
      <Dialog.Body className="overflow-y-auto min-h-0 break-words">
        <h3 className="font-medium mb-2">Answer</h3>
        <p className="whitespace-pre-wrap">{evidence.answer || "No answer found."}</p>
        <dl className="grid grid-cols-2 gap-2 my-4 text-sm">
          <dt>Stop reason</dt><dd>{evidence.stopped_reason?.replaceAll("_", " ") || "Not recorded"}</dd>
          <dt>Research cost</dt><dd>{typeof evidence.cost_usd === "number" && Number.isFinite(evidence.cost_usd) ? `$${evidence.cost_usd.toFixed(6)}` : "Not recorded"}</dd>
        </dl>
        {evidence.synthesis_fallback && <p className="mb-4">Fallback synthesis was used. Structured citations may be unavailable.</p>}
        <h3 className="font-medium mb-2">Sources</h3>
        {!evidence.citations.length && <p>No supporting citations recorded. Do not treat this answer as verified.</p>}
        <ol className="space-y-4">{evidence.citations.map((citation, index) => {
          const href = citationHref(citation.url)
          return <li key={`${index}:${citation.url}`} className="border-t border-border pt-3">
            <p className="font-medium">{citation.title || `Source ${index + 1}`}</p>
            <p className="text-sm">Fetched: {citation.fetched_at && Number.isFinite(Date.parse(citation.fetched_at)) ? new Date(citation.fetched_at).toLocaleString() : "Not recorded"}</p>
            {href ? <a href={href} target="_blank" rel="noopener noreferrer" className="underline break-all focus-visible:outline-2 focus-visible:outline-ring">{citation.url}<span className="sr-only"> (opens in a new tab)</span></a> : <p className="break-all">{citation.url} — link unavailable</p>}
            {citation.quoted_text && <blockquote className="border-l-2 border-border pl-3 mt-2 whitespace-pre-wrap">{citation.quoted_text}</blockquote>}
          </li>
        })}</ol>
      </Dialog.Body>
      <Dialog.Footer><Dialog.Close render={<Button />}>Close</Dialog.Close></Dialog.Footer>
    </Dialog.Popup>
  </Dialog.Root>
}
