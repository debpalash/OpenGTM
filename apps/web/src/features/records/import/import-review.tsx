import { CheckCircle2, Download, FileSpreadsheet } from "lucide-react"
import { Button } from "@/design-system/primitives"
import { downloadData, type ImportResult, type RecordKind } from "@/lib/records"

export function ImportReview({
  result,
  complete,
  filename,
  source,
  kind,
  onOpenRecord,
  onSelectImported
}: {
  result: ImportResult
  complete: boolean
  filename: string
  source: string
  kind: RecordKind
  onOpenRecord: (id: string) => void
  onSelectImported: (ids: string[]) => void
}) {
  const rejected = result.rows.filter((row) => row.status === "rejected")
  const accepted = result.rows.filter((row) => row.status === "accepted")
  const uniqueIDs = [
    ...new Set(accepted.flatMap((row) => (row.id ? [row.id] : [])))
  ]
  return (
    <section aria-label={complete ? "Import results" : "Import review"}>
      <div className="record-import-result-title">
        {complete ? (
          <CheckCircle2 aria-hidden="true" />
        ) : (
          <FileSpreadsheet aria-hidden="true" />
        )}
        <div>
          <h2>
            {complete
              ? result.replayed
                ? "Completed batch recovered"
                : "Import complete"
              : "Review your import"}
          </h2>
          <p>
            {filename} · {source}
          </p>
        </div>
      </div>
      <dl className="record-import-totals">
        <div>
          <dt>{complete ? "Accepted rows" : "Ready to import"}</dt>
          <dd>{result.imported.toLocaleString()}</dd>
        </div>
        <div>
          <dt>Rejected rows</dt>
          <dd>{result.rejected.toLocaleString()}</dd>
        </div>
        <div>
          <dt>Total rows</dt>
          <dd>{result.total.toLocaleString()}</dd>
        </div>
      </dl>
      <p className="record-import-hint">
        {complete
          ? "Accepted rows created or updated records. Multiple rows may resolve to one record."
          : "Accepted rows create or update matching records. Rejected rows are skipped. Manually reviewed facts keep their ownership."}
      </p>
      <p className="record-import-hint">
        No enrichment starts with this import.
      </p>
      {result.warnings.map((warning) => (
        <p key={warning} className="record-import-warning">
          {warning}
        </p>
      ))}
      {rejected.length > 0 && (
        <>
          <h3 className="import-rejection-title">Rows needing attention</h3>
          <div
            className="record-import-errors"
            role="region"
            aria-label="Rejected rows"
            tabIndex={0}
          >
            {rejected.slice(0, 50).map((row) => (
              <p key={row.row}>
                <strong>Row {row.row}</strong> · {row.error}
              </p>
            ))}
          </div>
          {rejected.length > 50 && (
            <p className="record-import-hint">
              Showing 50 of {rejected.length.toLocaleString()} rejected rows.
              Download the report for every row.
            </p>
          )}
        </>
      )}
      <Button
        variant="ghost"
        size="sm"
        onClick={() =>
          downloadData(JSON.stringify(result, null, 2), "import-report.json")
        }
      >
        <Download aria-hidden="true" />
        Download row report
      </Button>
      {complete && uniqueIDs.length > 0 && (
        <section
          className="import-connected-records"
          aria-label="Imported records"
        >
          <h3>
            {uniqueIDs.length.toLocaleString()} connected{" "}
            {kind === "company"
              ? uniqueIDs.length === 1
                ? "company"
                : "companies"
              : uniqueIDs.length === 1
                ? "person"
                : "people"}
          </h3>
          <div>
            {accepted.slice(0, 5).map((row) => (
              <Button
                key={row.row}
                size="sm"
                variant="outline"
                onClick={() => onOpenRecord(row.id!)}
              >
                View row {row.row} record
              </Button>
            ))}
          </div>
          <Button variant="outline" onClick={() => onSelectImported(uniqueIDs)}>
            Select records for enrichment
          </Button>
          <p>
            Selects this batch in the record library. Review providers and cost
            before running enrichment.
          </p>
        </section>
      )}
    </section>
  )
}
