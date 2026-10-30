import { useDeferredValue, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { Button, Field, Input } from "@/design-system/primitives"
import { ErrorState, ViewState } from "@/design-system/patterns/workbench"
import { fetchRecordPage, displayField } from "@/lib/records"
import { getActiveWorkspace } from "@/lib/auth"
import { currentTitle } from "./record-draft"

export function CompanyPeople({ companyId }: { companyId: string }) {
  const [offset, setOffset] = useState(0)
  const [search, setSearch] = useState("")
  const deferred = useDeferredValue(search)
  const query = useQuery({
    queryKey: [
      "company-people",
      getActiveWorkspace(),
      companyId,
      deferred,
      offset
    ],
    queryFn: async ({ signal }) => {
      const result = await fetchRecordPage(
        "person",
        {
          company_id: companyId,
          limit: 50,
          offset,
          search: deferred,
          sort: "name"
        },
        signal
      )
      if (result.records.some((person) => person.company_id !== companyId))
        throw new Error(
          "The returned employment links could not be verified for this company. Try loading the view again."
        )
      return result
    }
  })
  return (
    <section
      className="record-company-people"
      aria-labelledby="record-company-people-title"
    >
      <h2 id="record-company-people-title">People at this company</h2>
      <p>
        Current employment links. Previous employment remains in each person’s
        history.
      </p>
      <Field label="Search people at this company">
        {(control) => (
          <Input
            {...control}
            value={search}
            onChange={(event) => {
              setSearch(event.target.value)
              setOffset(0)
            }}
          />
        )}
      </Field>
      {query.isPending ? (
        <ViewState kind="loading" title="Loading people…" />
      ) : query.isError ? (
        <ErrorState error={query.error} retry={() => void query.refetch()} />
      ) : (
        query.data && (
          <>
            <p role="status">
              {query.data.total.toLocaleString()} matching{" "}
              {query.data.total === 1 ? "person" : "people"}
            </p>
            <ul>
              {query.data.records.map((person) => (
                <li key={person.id}>
                  <Link to={`/people?record=${encodeURIComponent(person.id)}`}>
                    <strong>
                      {displayField(person.fields.full_name) ||
                        "Unnamed person"}
                    </strong>
                    <span>{currentTitle(person) || "Role not provided"}</span>
                    <span>
                      {displayField(person.fields.email) ||
                        "Work email not provided"}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
            {query.data.records.length === 0 && (
              <p>
                {offset
                  ? "No people on this page. Return to the previous page."
                  : search
                    ? "No people match this search."
                    : "No people are currently linked to this company."}
              </p>
            )}
            <div className="record-company-people-paging">
              <Button
                size="sm"
                disabled={offset === 0 || query.isFetching}
                onClick={() => setOffset((value) => Math.max(0, value - 50))}
              >
                Previous people
              </Button>
              <Button
                size="sm"
                disabled={offset + 50 >= query.data.total || query.isFetching}
                onClick={() => setOffset((value) => value + 50)}
              >
                Next people
              </Button>
            </div>
          </>
        )
      )}
    </section>
  )
}
