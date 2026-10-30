import { useDeferredValue, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Field, Input, NativeSelect } from "@/design-system/primitives"
import { fetchRecordPage, displayField } from "@/lib/records"
import { getActiveWorkspace } from "@/lib/auth"

export function CompanyPicker({
  value,
  name,
  knownEmployer,
  disabled,
  onChange
}: {
  value: string
  name: string
  knownEmployer: boolean
  disabled: boolean
  onChange: (id: string, name: string) => void
}) {
  const [search, setSearch] = useState("")
  const deferred = useDeferredValue(search)
  const query = useQuery({
    queryKey: ["record-companies-picker", getActiveWorkspace(), deferred],
    queryFn: ({ signal }) =>
      fetchRecordPage("company", { limit: 20, search: deferred }, signal)
  })
  const records = query.data?.records ?? []
  return (
    <div className="record-company-picker">
      <Field
        label="Search companies"
        description="Search the full company collection; shows up to 20 matches."
      >
        {(control) => (
          <Input
            {...control}
            value={search}
            disabled={disabled}
            onKeyDown={(event) => {
              if (event.key === "Enter") event.preventDefault()
            }}
            onChange={(event) => setSearch(event.target.value)}
          />
        )}
      </Field>
      <Field
        id="record-current-company"
        label="Current company"
        description={
          knownEmployer
            ? "To remove the current employer, choose End current employment below."
            : "An employer can be added later; a role may exist on its own."
        }
      >
        {(control) => (
          <NativeSelect
            {...control}
            value={value}
            disabled={disabled}
            onChange={(event) =>
              onChange(
                event.target.value,
                displayField(
                  records.find((record) => record.id === event.target.value)
                    ?.fields.name
                )
              )
            }
          >
            <option value="" disabled={knownEmployer}>
              Employer unknown
            </option>
            {value && !records.some((record) => record.id === value) && (
              <option value={value}>{name || "Selected company"}</option>
            )}
            {records.map((record) => (
              <option key={record.id} value={record.id}>
                {displayField(record.fields.name)} ·{" "}
                {displayField(record.fields.domain)}
              </option>
            ))}
          </NativeSelect>
        )}
      </Field>
      {query.isFetching && <p role="status">Searching companies…</p>}
      {query.isError && (
        <p role="alert">
          Companies could not be loaded. The selected employer and draft are
          kept.{" "}
          <button type="button" onClick={() => void query.refetch()}>
            Try again
          </button>
        </p>
      )}
      {!query.isFetching && !query.isError && query.data?.total === 0 && (
        <p>No companies match this search.</p>
      )}
    </div>
  )
}
