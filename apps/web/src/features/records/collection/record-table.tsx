import { Link } from "react-router-dom"
import { ArrowUpRight, CheckCircle2 } from "lucide-react"
import { StatusBadge } from "@/design-system/patterns/workbench"
import { relativeTime } from "@/lib/record-overview"
import { displayField, type RecordKind, type SharedRecord } from "@/lib/records"
import { visibleColumns, type ColumnLayout } from "./columns"
import {
  currentCompany,
  currentRole,
  emailState,
  factText,
  recordLocation,
  recordName,
  safeExternalURL
} from "./facts"
import { RecordCompleteness } from "./completeness"
import "./collection.css"

export function RecordTable({
  kind,
  records,
  total,
  density,
  layout,
  selected,
  onToggle,
  onTogglePage,
  onInspect
}: {
  kind: RecordKind
  records: SharedRecord[]
  total: number
  density: string
  layout: ColumnLayout
  selected: ReadonlySet<string>
  onToggle: (id: string) => void
  onTogglePage: (checked: boolean) => void
  onInspect: (record: SharedRecord, trigger: HTMLButtonElement) => void
}) {
  const columns = visibleColumns(kind, layout)
  const checked = records.filter((record) => selected.has(record.id)).length
  const plural = kind === "company" ? "Companies" : "People"
  return (
    <div
      className="records-table-scroll"
      role="region"
      aria-label={`${plural} table, scroll for more columns`}
      tabIndex={0}
    >
      <table className="records-table" data-density={density}>
        <caption className="sr-only">
          {plural}, {total.toLocaleString()} matching records. Select a name to
          inspect shared facts and history.
        </caption>
        <thead>
          <tr>
            <th scope="col" className="records-check">
              <label className="gtm-checkbox-target">
                <input
                  type="checkbox"
                  aria-label="Select all records on this page"
                  checked={checked === records.length && records.length > 0}
                  ref={(element) => {
                    if (element)
                      element.indeterminate =
                        checked > 0 && checked < records.length
                  }}
                  onChange={(event) => onTogglePage(event.target.checked)}
                />
              </label>
            </th>
            {columns.map((column) => (
              <th
                key={column.id}
                scope="col"
                className={
                  column.id === "identity" ? "records-identity" : undefined
                }
              >
                {column.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {records.map((record) => (
            <tr key={record.id} data-selected={selected.has(record.id)}>
              <td className="records-check">
                <label className="gtm-checkbox-target">
                  <input
                    type="checkbox"
                    aria-label={`Select ${recordName(record)}`}
                    checked={selected.has(record.id)}
                    onChange={() => onToggle(record.id)}
                  />
                </label>
              </td>
              {columns.map((column) => (
                <td
                  key={column.id}
                  data-column={column.id}
                  className={
                    column.id === "identity" ? "records-identity" : undefined
                  }
                >
                  <RecordCell
                    column={column.id}
                    record={record}
                    onInspect={onInspect}
                  />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
function RecordCell({
  column,
  record,
  onInspect
}: {
  column: string
  record: SharedRecord
  onInspect: (record: SharedRecord, trigger: HTMLButtonElement) => void
}) {
  const fields = record.fields
  switch (column) {
    case "identity":
      return (
        <button
          className="records-name"
          onClick={(event) => onInspect(record, event.currentTarget)}
        >
          <span
            aria-hidden="true"
            className="records-avatar"
            data-person={record.record_type === "person"}
          >
            {recordName(record)
              .split(/\s+/)
              .slice(0, 2)
              .map((part) => Array.from(part)[0])
              .join("")
              .toUpperCase() || "?"}
          </span>
          <span>{recordName(record)}</span>
          <ArrowUpRight aria-hidden="true" />
        </button>
      )
    case "domain":
      return (
        <span className="records-domain">
          {displayField(fields.domain) || (
            <span className="records-missing">No domain</span>
          )}
        </span>
      )
    case "industry":
      return (
        <span>
          {factText(fields.industry) || factText(fields.specialization) || (
            <span className="records-missing">—</span>
          )}
        </span>
      )
    case "company_role": {
      const company = currentCompany(record)
      const role = currentRole(record)
      return (
        <div className="records-two-line">
          {company.id ? (
            <Link to={`/companies?record=${encodeURIComponent(company.id)}`}>
              {company.name || "Unnamed company"}
            </Link>
          ) : (
            <span className="records-employer">
              {company.name || "Company unknown"}
            </span>
          )}
          {role && <span>{role}</span>}
        </div>
      )
    }
    case "email": {
      const state = emailState(record)
      const label = {
        verified: "Verified",
        invalid: "Invalid",
        risky: "Risky",
        unverified: "Not verified",
        missing: "Not found"
      }[state]
      return (
        <span className="records-email">
          {state === "missing" ? (
            <span className="records-missing">Not found</span>
          ) : (
            <>
              <span>{displayField(fields.email)}</span>
              <span className="record-email-state" data-state={state}>
                {state === "verified" && <CheckCircle2 aria-hidden="true" />}
                {label}
              </span>
            </>
          )}
        </span>
      )
    }
    case "fit":
      return (
        <StatusBadge
          tone={
            fields.score_tier === "hot"
              ? "positive"
              : fields.score_tier === "warm"
                ? "accent"
                : "neutral"
          }
        >
          {{
            hot: "High fit",
            warm: "Potential fit",
            cold: "Low fit",
            unqualified: "Unqualified"
          }[String(fields.score_tier)] || "Not scored"}
          {fields.score != null && (
            <span className="records-score">{displayField(fields.score)}</span>
          )}
        </StatusBadge>
      )
    case "location":
      return (
        <span className="records-secondary">
          {recordLocation(record) || "—"}
        </span>
      )
    case "updated":
      return (
        <span className="records-secondary">
          {record.updated_at ? relativeTime(record.updated_at) : "—"}
        </span>
      )
    case "completeness":
      return <RecordCompleteness record={record} />
    case "phone":
      return (
        <span className="records-secondary">
          {displayField(fields.phone) || "—"}
        </span>
      )
    case "description":
      return (
        <span
          className="record-cell-description"
          title={factText(fields.description)}
        >
          {factText(fields.description) || "—"}
        </span>
      )
    case "employees":
      return (
        <span className="records-secondary">
          {factText(fields.employee_count_exact) || "—"}
        </span>
      )
    case "linkedin": {
      const value = factText(fields.linkedin_url)
      const url = safeExternalURL(value)
      return url ? (
        <a
          className="record-external-link"
          href={url}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={`LinkedIn for ${recordName(record)} (opens in a new tab)`}
        >
          LinkedIn
          <ArrowUpRight aria-hidden="true" />
        </a>
      ) : (
        <span className="records-secondary">{value || "—"}</span>
      )
    }
    default:
      return null
  }
}
