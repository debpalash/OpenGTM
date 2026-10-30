import { useState } from "react"
import { Button, Field, NativeSelect } from "@/design-system/primitives"
import { displayField, type RecordKind, type SharedRecord } from "@/lib/records"
import {
  recordConflictRows,
  resolveRecordConflict,
  type RecordDraft
} from "./record-draft"

export function RecordConflictReview({
  kind,
  baseline,
  draft,
  latest,
  label,
  onResolved,
  onCancel
}: {
  kind: RecordKind
  baseline: SharedRecord
  draft: RecordDraft
  latest: SharedRecord
  label: (key: string) => string
  onResolved: (draft: RecordDraft) => void
  onCancel: () => void
}) {
  const rows = recordConflictRows(kind, baseline, draft, latest)
  const [choices, setChoices] = useState<Record<string, "draft" | "current">>(
    {}
  )
  const missing = rows.filter(
    (row) => row.needsChoice && !choices[row.key]
  ).length
  const value = (
    key: string,
    item: unknown,
    source: "before" | "current" | "draft"
  ) => {
    if (key !== "$employment" || !item || typeof item !== "object")
      return displayField(item) || "Not provided"
    const data = item as { companyId: string; title: string; ended: boolean }
    const company = source === "current" ? latest : baseline
    const companyName =
      source === "draft"
        ? draft.companyName
        : displayField(company.company?.name)
    return data.ended
      ? "Employment ended"
      : `${companyName || data.companyId || "Employer unknown"}\n${data.title || "Role not provided"}`
  }
  return (
    <section
      className="record-conflict"
      aria-labelledby="record-conflict-title"
    >
      <h2 id="record-conflict-title">Review changes before saving</h2>
      <p role="status">
        The record changed from version {baseline.version} to {latest.version}.
        Your draft is kept. Choose a value wherever both versions changed.
      </p>
      {rows.map((row) => (
        <article key={row.key} className="record-conflict-field">
          <h3>
            {row.key === "$employment"
              ? "Current employment and role"
              : label(row.key)}
          </h3>
          <dl>
            <div>
              <dt>When you started</dt>
              <dd>{value(row.key, row.before, "before")}</dd>
            </div>
            <div>
              <dt>Current shared value</dt>
              <dd>{value(row.key, row.current, "current")}</dd>
            </div>
            <div>
              <dt>Your draft</dt>
              <dd>{value(row.key, row.draft, "draft")}</dd>
            </div>
          </dl>
          {row.needsChoice ? (
            <Field
              label={`Choose value for ${row.key === "$employment" ? "current employment" : label(row.key)}`}
            >
              {(control) => (
                <NativeSelect
                  {...control}
                  value={choices[row.key] ?? ""}
                  onChange={(event) =>
                    setChoices((previous) => ({
                      ...previous,
                      [row.key]: event.target.value as "draft" | "current"
                    }))
                  }
                >
                  <option value="">Choose a value…</option>
                  <option value="draft">Keep my draft</option>
                  <option value="current">Use current shared value</option>
                </NativeSelect>
              )}
            </Field>
          ) : (
            <p className="record-conflict-outcome">
              {row.mineChanged
                ? "Your change is retained."
                : "The current shared value is retained."}
            </p>
          )}
        </article>
      ))}
      <p className="record-tab-description">
        Continuing prepares a draft against version {latest.version}. Nothing is
        saved until you choose Save shared record.
      </p>
      <div className="record-conflict-actions">
        <Button variant="outline" onClick={onCancel}>
          Back to my draft
        </Button>
        <Button
          variant="solid"
          disabled={missing > 0}
          onClick={() =>
            onResolved(
              resolveRecordConflict(kind, baseline, draft, latest, choices)
            )
          }
        >
          Continue with reviewed draft
        </Button>
      </div>
    </section>
  )
}
