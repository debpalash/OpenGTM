// Readable evidence for one plugin result: where it was fetched, when, with
// which selectors or mappings, and the SHA-256 of the exact bytes used.

import { useState, type ReactNode } from "react"
import { Check, Copy, ExternalLink, FileSearch } from "lucide-react"
import { Button } from "@/components/ui/button"
import { absoluteTime, formatBytes, formatCost, relativeTime } from "@/lib/plugin-format"
import { normalizeEvidence, safeHttpUrl, type EvidenceSource } from "@/lib/plugin-evidence"

function CopyHash({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  return <span className="inline-flex min-w-0 items-center gap-1">
    <code className="min-w-0 truncate font-mono text-[11px]" title={value}>{value}</code>
    <Button type="button" variant="ghost" size="icon-xs" aria-label={copied ? "Copied SHA-256" : "Copy SHA-256"}
      onClick={() => {
        void navigator.clipboard?.writeText(value).then(() => {
          setCopied(true)
          window.setTimeout(() => setCopied(false), 1500)
        }).catch(() => {})
      }}>
      {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
    </Button>
  </span>
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return <div className="grid grid-cols-[6.5rem_minmax(0,1fr)] items-baseline gap-2">
    <dt className="text-[11px] text-muted-foreground">{label}</dt>
    <dd className="min-w-0 text-xs">{children}</dd>
  </div>
}

function SourceBlock({ source, index, count }: { source: EvidenceSource; index: number; count: number }) {
  const href = safeHttpUrl(source.url)
  return <section className="grid gap-1.5 rounded-md border bg-background/60 p-3" aria-label={count > 1 ? `Source ${index + 1} of ${count}` : "Source"}>
    <dl className="grid gap-1.5">
      <Row label="Source URL">
        {href
          ? <a href={href} target="_blank" rel="noopener noreferrer nofollow" className="inline-flex max-w-full items-center gap-1 break-all text-[var(--gtm-accent)] hover:underline">
            <span className="min-w-0 break-all">{source.url}</span><ExternalLink aria-hidden="true" className="size-3 shrink-0" /><span className="sr-only">(opens in a new tab)</span>
          </a>
          : <span className="break-all">{source.url ?? "—"}</span>}
      </Row>
      {source.fetchedAt && <Row label="Fetched">
        <time dateTime={source.fetchedAt}>{absoluteTime(source.fetchedAt)}</time> <span className="text-muted-foreground">({relativeTime(source.fetchedAt)})</span>
      </Row>}
      {(source.method || source.status !== undefined) && <Row label="Response">
        {[source.method, source.status !== undefined && `HTTP ${source.status}`, source.contentType, source.bytes !== undefined && formatBytes(source.bytes)].filter(Boolean).join(" · ")}
      </Row>}
      {source.page !== undefined && <Row label="Page">{source.page}</Row>}
      {source.sha256 && <Row label="SHA-256"><CopyHash value={source.sha256} /></Row>}
    </dl>
  </section>
}

export function EvidencePanel({ evidence }: { evidence: unknown }) {
  const view = normalizeEvidence(evidence)
  const empty = !view.sources.length && !view.selectors.length && view.confidence === undefined && view.costUsd === undefined
  return <div className="grid gap-3 text-left whitespace-normal">
    <h4 className="flex items-center gap-1.5 text-xs font-semibold"><FileSearch aria-hidden="true" className="size-3.5" />Evidence</h4>
    {empty && <p className="text-xs text-muted-foreground">{evidence == null ? "The plugin attached no evidence to this record." : "Evidence is in an unrecognized format; the raw JSON is below."}</p>}
    {view.sources.map((source, index) => <SourceBlock key={index} source={source} index={index} count={view.sources.length} />)}
    {(view.itemsSelector || view.selectors.length > 0) && <section className="grid gap-1.5" aria-label={view.selectorKind === "mapping" ? "Field mappings" : "Selectors"}>
      <h5 className="text-[11px] font-medium text-muted-foreground">{view.selectorKind === "mapping" ? "Field mappings" : "Selectors"}</h5>
      <dl className="grid gap-1 rounded-md border bg-background/60 p-3">
        {view.itemsSelector && <Row label="Record items"><code className="font-mono text-[11px] break-all">{view.itemsSelector}</code></Row>}
        {view.selectors.map(selector => <Row key={selector.field} label={selector.field}><code className="font-mono text-[11px] break-all">{selector.expression}</code></Row>)}
      </dl>
    </section>}
    {(view.confidence !== undefined || view.costUsd !== undefined) && <dl className="grid gap-1.5">
      {view.confidence !== undefined && <Row label="Confidence">{Math.round(view.confidence * 100)}%</Row>}
      {view.costUsd !== undefined && <Row label="Cost">{formatCost(view.costUsd)}</Row>}
    </dl>}
    {view.pluginReported !== undefined && <details className="text-xs">
      <summary className="cursor-pointer text-muted-foreground">Plugin-reported evidence (informational)</summary>
      <pre className="mt-1 max-h-48 overflow-auto rounded-md bg-muted p-2 font-mono text-[11px]">{JSON.stringify(view.pluginReported, null, 2)}</pre>
    </details>}
    {evidence != null && <details className="text-xs">
      <summary className="cursor-pointer text-muted-foreground">Raw evidence JSON</summary>
      <pre className="mt-1 max-h-64 overflow-auto rounded-md bg-muted p-2 font-mono text-[11px]">{JSON.stringify(evidence, null, 2)}</pre>
    </details>}
  </div>
}
