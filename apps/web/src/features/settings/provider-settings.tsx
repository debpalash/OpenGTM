import "./provider-settings.css"
import { useState } from "react"
import { Search } from "lucide-react"
import { useProviders } from "@/lib/hooks"
import { Label } from "@/components/ui/label"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { NativeSelect } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorState } from "@/design-system/patterns/workbench"
import { ProviderCard } from "./provider-card"

export function ProviderSettingsPanel() {
  const { data, isLoading, isError, error, refetch } = useProviders()
  const [search, setSearch] = useState("")
  const [filter, setFilter] = useState("all")
  const providers = data?.providers ?? []
  const query = search.trim().toLocaleLowerCase()
  const matching = new Set(
    providers
      .filter(
        (provider) =>
          (filter === "all" ||
            provider.configured === (filter === "configured")) &&
          `${provider.name} ${provider.model} ${provider.free_tier}`
            .toLocaleLowerCase()
            .includes(query)
      )
      .map((provider) => provider.id)
  )
  const sorted = [...providers].sort(
    (a, b) =>
      Number(b.is_default) - Number(a.is_default) ||
      Number(b.configured) - Number(a.configured) ||
      a.name.localeCompare(b.name)
  )

  return (
    <section
      aria-labelledby="provider-settings-title"
      className="provider-settings space-y-3"
    >
      <div>
        <h2 id="provider-settings-title" className="text-sm font-medium">
          LLM Providers
        </h2>
        <p className="text-xs text-muted-foreground mt-1">
          Instance-wide models for extraction, scoring, and outreach. An
          instance admin manages these settings across workspaces.
        </p>
      </div>
      {isError && data && (
        <div role="alert" className="text-xs text-destructive">
          The latest provider settings could not be loaded. Showing the last
          confirmed settings.{" "}
          <button
            type="button"
            className="underline"
            onClick={() => void refetch()}
          >
            Try again
          </button>
        </div>
      )}
      {isError && !data ? (
        <ErrorState error={error} retry={() => void refetch()} />
      ) : isLoading ? (
        <div
          className="grid gap-3 md:grid-cols-2"
          aria-label="Loading provider settings"
        >
          {Array.from({ length: 6 }, (_, i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      ) : (
        <>
          <div className="flex flex-wrap items-end gap-2 rounded-md border p-2">
            <div className="min-w-44 flex-1 space-y-1">
              <Label htmlFor="provider-search" className="text-xs">
                Search AI providers
              </Label>
              <div className="relative">
                <Search
                  aria-hidden="true"
                  className="pointer-events-none absolute left-2 top-2.5 size-3.5 text-muted-foreground"
                />
                <Input
                  id="provider-search"
                  value={search}
                  onChange={(event) => setSearch(event.target.value)}
                  className="pl-7"
                  placeholder="Provider, model, or plan"
                />
              </div>
            </div>
            <div className="space-y-1">
              <Label htmlFor="provider-filter" className="text-xs">
                Configuration
              </Label>
              <NativeSelect
                id="provider-filter"
                value={filter}
                onChange={(event) => setFilter(event.target.value)}
              >
                <option value="all">All providers</option>
                <option value="configured">Configured</option>
                <option value="available">Not configured</option>
              </NativeSelect>
            </div>
            <p role="status" className="pb-2 text-xs text-muted-foreground">
              {matching.size} of {providers.length} providers
            </p>
          </div>
          {providers.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">
              No AI providers are available.
            </p>
          ) : (
            matching.size === 0 && (
              <div className="rounded-md border p-4 text-sm">
                <p>No providers match these filters.</p>
                <Button
                  variant="outline"
                  size="sm"
                  className="mt-2"
                  onClick={() => {
                    setSearch("")
                    setFilter("all")
                  }}
                >
                  Clear provider filters
                </Button>
              </div>
            )
          )}
          <div className="grid gap-3 md:grid-cols-2">
            {sorted.map((provider) => (
              <div key={provider.id} hidden={!matching.has(provider.id)}>
                <ProviderCard provider={provider} />
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  )
}
