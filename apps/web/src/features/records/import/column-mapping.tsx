import { useId, useState } from "react"
import { Button, Input } from "@/design-system/primitives"
import type { ImportInspection } from "@/lib/record-import"
import type { RecordKind } from "@/lib/records"
import {
  importTargets,
  mappingGroup,
  mappingIssues,
  mappingWarnings,
  MAPPING_GROUPS
} from "./mapping"

export function ColumnMapping({
  inspection,
  mapping,
  kind,
  disabled,
  onChange
}: {
  inspection: ImportInspection
  mapping: Record<string, string>
  kind: RecordKind
  disabled: boolean
  onChange: (name: string, value: string) => void
}) {
  const id = useId(),
    [search, setSearch] = useState("")
  // Keep inputs in place while typing. Regroup only on an explicit action;
  // moving a keyed row between parent sections otherwise destroys its focus.
  const [groups, setGroups] = useState(() =>
    Object.fromEntries(
      inspection.columns.map((column) => [
        column.name,
        mappingGroup(mapping[column.name] ?? column.target, kind)
      ])
    )
  )
  const needsRegroup = inspection.columns.some(
    (column) =>
      groups[column.name] !==
      mappingGroup(mapping[column.name] ?? column.target, kind)
  )
  const issues = mappingIssues(inspection.columns, mapping, kind)
  const warnings = mappingWarnings(inspection.columns, mapping, kind)
  const visible = inspection.columns.filter((column) =>
    `${column.name} ${mapping[column.name]}`
      .toLocaleLowerCase()
      .includes(search.toLocaleLowerCase())
  )
  return (
    <section className="import-column-task" aria-label="Column mapping">
      <div className="record-import-section">
        <h2>Match your columns</h2>
        <p>Choose a field, or clear to ignore.</p>
      </div>
      <div className="import-mapping-summary">
        <span>
          {inspection.rows.toLocaleString()} rows · {inspection.columns.length}{" "}
          columns
        </span>
        <span
          role="status"
          className={issues.size ? "import-mapping-issue" : undefined}
        >
          {issues.size
            ? `${issues.size} columns need attention`
            : "Mappings ready to validate"}
        </span>
      </div>
      <div className="import-mapping-tools">
        <label className="import-mapping-search">
          <span className="sr-only">Find a source column or target field</span>
          <Input
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="Find a column…"
            type="search"
          />
        </label>
        <Button
          variant="outline"
          size="sm"
          disabled={disabled || !needsRegroup}
          onClick={() =>
            setGroups(
              Object.fromEntries(
                inspection.columns.map((column) => [
                  column.name,
                  mappingGroup(mapping[column.name] ?? column.target, kind)
                ])
              )
            )
          }
        >
          Regroup fields
        </Button>
      </div>
      <datalist id={`${id}-targets`}>
        {importTargets(kind).map((target) => (
          <option key={target} value={target} />
        ))}
      </datalist>
      {warnings.map((warning) => (
        <p key={warning} className="record-import-warning">
          {warning}
        </p>
      ))}
      {MAPPING_GROUPS.map((group) => {
        const columns = visible.filter(
          (column) => groups[column.name] === group
        )
        if (!columns.length) return null
        return (
          <section
            key={group}
            className="import-mapping-group"
            aria-label={`${group} mappings`}
          >
            <h3>
              {group}
              <span>{columns.length}</span>
            </h3>
            {columns.map((column) => {
              const index = inspection.columns.indexOf(column),
                target = mapping[column.name] ?? column.target
              const issue = issues.get(column.name),
                changed = target !== column.target
              return (
                <div className="import-mapping-row" key={column.name}>
                  <div className="import-source-column">
                    <strong>{column.name}</strong>
                    <span className="import-match-kind">
                      {!target
                        ? "Ignored"
                        : changed
                          ? "Your mapping"
                          : {
                              matched: "Recognized header",
                              explicit: "Explicit field",
                              custom: "Custom field",
                              chosen: "Saved mapping"
                            }[column.match]}
                      {column.precision_sensitive ? " · Exact ID" : ""}
                    </span>
                    {column.samples.length > 0 && (
                      <>
                        <code className="import-source-preview">
                          {column.samples[0].value || "(empty)"}
                          {column.samples[0].truncated ? "…" : ""}
                        </code>
                        <details>
                          <summary>
                            Source samples
                            {column.precision_sensitive ? " · exact ID" : ""}
                          </summary>
                          {column.samples.map((sample) => (
                            <p key={sample.row}>
                              <span>Row {sample.row}</span>
                              <code>
                                {sample.value || "(empty)"}
                                {sample.truncated ? "…" : ""}
                              </code>
                            </p>
                          ))}
                        </details>
                      </>
                    )}
                  </div>
                  <div className="import-target-column">
                    <label htmlFor={`${id}-${index}`}>
                      OpenGTM field · {mappingGroup(target, kind)}
                      <span className="sr-only"> for {column.name}</span>
                    </label>
                    <Input
                      id={`${id}-${index}`}
                      aria-label={`Map ${column.name}`}
                      aria-invalid={!!issue}
                      aria-describedby={
                        issue ? `${id}-${index}-error` : undefined
                      }
                      list={`${id}-targets`}
                      value={target}
                      placeholder="Ignore column"
                      disabled={disabled}
                      onChange={(event) =>
                        onChange(column.name, event.target.value)
                      }
                    />
                    {issue && (
                      <p
                        id={`${id}-${index}-error`}
                        className="import-mapping-issue"
                      >
                        {issue}
                      </p>
                    )}
                  </div>
                </div>
              )
            })}
          </section>
        )
      })}
      {!visible.length && (
        <p className="record-import-hint">
          No matching columns. Clear the search to see all mappings.
        </p>
      )}
      <p className="record-import-hint">
        Custom fields use company.field, person.field or employment.field.
      </p>
    </section>
  )
}
