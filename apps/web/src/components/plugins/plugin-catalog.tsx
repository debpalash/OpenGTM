// Plugin catalog: searchable, filterable cards with the capabilities a plugin
// declares (network destinations, secrets, browser). Capabilities are what a
// workspace trusts a plugin with, so every card shows them up front.

import { useMemo, useState } from "react"
import {
  AppWindow, Database, Globe, KeyRound, Play, Puzzle, Radar, ScanSearch, Send, ShieldAlert, ShieldCheck, ShieldX,
  Sigma, TriangleAlert, Wrench, type LucideIcon,
} from "lucide-react"
import { Empty } from "@/components/states"
import { useCanRole } from "@/components/gate"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { NativeSelect } from "@/components/ui/native-select"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import type { PluginCatalog, PluginInfo } from "@/lib/plugin-api"
import { broadNetworkPattern, filterPlugins, pluginTitle } from "@/lib/plugin-catalog"
import { cn } from "@/lib/utils"

const KIND_ICONS: Record<string, LucideIcon> = {
  scraper: ScanSearch, provider: Database, signal: Radar, destination: Send, function: Sigma, tool: Wrench,
}

function SignatureBadge({ signature }: { signature: string }) {
  if (signature === "trusted") {
    return <Badge variant="outline" className="gap-1 border-emerald-500/30 text-emerald-700 dark:text-emerald-300" title="Signed by a trusted publisher">
      <ShieldCheck aria-hidden="true" />Signed
    </Badge>
  }
  if (signature === "unsigned") {
    return <Badge variant="outline" className="gap-1 border-amber-500/40 text-amber-700 dark:text-amber-300" title="No signature; allowed by the optional signature policy">
      <ShieldAlert aria-hidden="true" />Unsigned
    </Badge>
  }
  return <Badge variant="destructive" className="gap-1" title={`Signature: ${signature}`}><ShieldX aria-hidden="true" />{signature.replaceAll("_", " ")}</Badge>
}

function Capabilities({ plugin }: { plugin: PluginInfo }) {
  return (
    <dl className="grid gap-1.5 text-xs" aria-label={`Capabilities of ${pluginTitle(plugin)}`}>
      <div className="flex items-start gap-2">
        <dt className="flex w-16 shrink-0 items-center gap-1 text-muted-foreground"><Globe aria-hidden="true" className="size-3" />Network</dt>
        <dd className="flex min-w-0 flex-wrap gap-1">
          {plugin.network.length === 0 ? <span className="text-muted-foreground">None</span> : plugin.network.map(pattern => {
            const warning = broadNetworkPattern(pattern)
            return <code key={pattern} title={warning ?? pattern}
              className={cn("max-w-full truncate rounded-sm bg-muted px-1 py-px font-mono text-[11px]", warning && "bg-amber-500/10 text-amber-800 dark:text-amber-200")}>
              {warning && <TriangleAlert aria-label="Broad access" className="mr-0.5 inline size-3 align-[-2px]" />}{pattern}
            </code>
          })}
        </dd>
      </div>
      <div className="flex items-start gap-2">
        <dt className="flex w-16 shrink-0 items-center gap-1 text-muted-foreground"><KeyRound aria-hidden="true" className="size-3" />Secrets</dt>
        <dd className="flex min-w-0 flex-wrap gap-1">
          {plugin.secrets.length === 0 ? <span className="text-muted-foreground">None</span>
            : plugin.secrets.map(secret => <code key={secret} className="rounded-sm bg-muted px-1 py-px font-mono text-[11px]">{secret}</code>)}
        </dd>
      </div>
      <div className="flex items-start gap-2">
        <dt className="flex w-16 shrink-0 items-center gap-1 text-muted-foreground"><AppWindow aria-hidden="true" className="size-3" />Browser</dt>
        <dd className={plugin.browser ? "font-medium text-amber-700 dark:text-amber-300" : "text-muted-foreground"}>{plugin.browser ? "Uses a headless browser" : "No"}</dd>
      </div>
    </dl>
  )
}

function RunButton({ plugin, onRun }: { plugin: PluginInfo; onRun: (plugin: PluginInfo) => void }) {
  const role = useCanRole("editor")
  const blocked = !plugin.runnable ? plugin.unrunnable_reason || "This plugin cannot run on this server yet." : role.allowed ? null : role.reason
  const button = <Button size="sm" className="gap-1" disabled={!!blocked} onClick={() => onRun(plugin)}
    aria-label={`Run ${pluginTitle(plugin)}${blocked ? ` (unavailable: ${blocked})` : ""}`}><Play aria-hidden="true" />Run</Button>
  if (!blocked) return button
  return <Tooltip>
    <TooltipTrigger render={<span className="inline-flex cursor-not-allowed" tabIndex={0} />}>{button}</TooltipTrigger>
    <TooltipContent>{blocked}</TooltipContent>
  </Tooltip>
}

export function PluginCard({ plugin, onRun }: { plugin: PluginInfo; onRun: (plugin: PluginInfo) => void }) {
  const Icon = KIND_ICONS[plugin.kind] ?? Puzzle
  return (
    <Card className="w-full gap-3 p-4" data-plugin={plugin.name}>
      <div className="flex items-start gap-3">
        <div className="flex size-8 shrink-0 items-center justify-center rounded-md bg-muted text-muted-foreground"><Icon aria-hidden="true" className="size-4" /></div>
        <div className="min-w-0 flex-1">
          <h3 className="truncate text-sm font-semibold">{pluginTitle(plugin)}</h3>
          <p className="truncate font-mono text-[11px] text-muted-foreground">{plugin.name}{plugin.version && <> · v{plugin.version}</>}</p>
        </div>
        <SignatureBadge signature={plugin.signature} />
      </div>
      <div className="flex flex-wrap gap-1">
        <Badge variant="secondary">{plugin.kind || "unknown kind"}</Badge>
        <Badge variant="secondary">{plugin.runtime || "unknown runtime"}</Badge>
        {plugin.source === "connector" && <Badge variant="outline" title="A v1 connector manifest loaded as a declarative provider">v1 connector</Badge>}
        {plugin.tags.slice(0, 3).map(tag => <Badge key={tag} variant="ghost" className="text-muted-foreground">#{tag}</Badge>)}
      </div>
      <p className={cn("line-clamp-3 text-xs", plugin.description ? "text-foreground/80" : "text-muted-foreground italic")}>{plugin.description || "No description provided."}</p>
      <Capabilities plugin={plugin} />
      <div className="mt-auto flex items-center justify-between gap-2 border-t pt-3">
        <p className="min-w-0 truncate text-[11px] text-muted-foreground">
          {[plugin.author && `by ${plugin.author}`, plugin.license].filter(Boolean).join(" · ") || "Author not declared"}
        </p>
        <RunButton plugin={plugin} onRun={onRun} />
      </div>
      {!plugin.runnable && <p className="-mt-1 text-[11px] text-muted-foreground">{plugin.unrunnable_reason || "Not runnable on this server yet."}</p>}
    </Card>
  )
}

export function PluginCatalogView({ catalog, onRun }: { catalog: PluginCatalog; onRun: (plugin: PluginInfo) => void }) {
  const [query, setQuery] = useState("")
  const [kind, setKind] = useState("all")
  const [runtime, setRuntime] = useState("all")
  const kinds = useMemo(() => [...new Set(catalog.plugins.map(p => p.kind).filter(Boolean))].sort(), [catalog.plugins])
  const runtimes = useMemo(() => [...new Set(catalog.plugins.map(p => p.runtime).filter(Boolean))].sort(), [catalog.plugins])
  const visible = useMemo(() => filterPlugins(catalog.plugins, query, kind, runtime)
    .sort((a, b) => pluginTitle(a).localeCompare(pluginTitle(b))), [catalog.plugins, query, kind, runtime])
  const filtered = query !== "" || kind !== "all" || runtime !== "all"

  return (
    <div className="flex flex-col gap-3">
      {catalog.errors.length > 0 && <Alert variant="destructive">
        <TriangleAlert aria-hidden="true" />
        <AlertTitle>{catalog.errors.length === 1 ? "1 plugin failed to load" : `${catalog.errors.length} plugins failed to load`}</AlertTitle>
        <AlertDescription>
          <details>
            <summary className="cursor-pointer text-xs">Show details (visible to admins only)</summary>
            <ul className="mt-1 space-y-1 text-xs">
              {catalog.errors.map(error => <li key={error.path}><code className="font-mono">{error.path}</code>: {error.error}</li>)}
            </ul>
          </details>
        </AlertDescription>
      </Alert>}
      <div className="flex flex-wrap items-center gap-2">
        <label className="min-w-48 flex-1 sm:max-w-xs">
          <span className="sr-only">Search plugins</span>
          <Input type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder="Search plugins" />
        </label>
        <NativeSelect aria-label="Kind" value={kind} onChange={event => setKind(event.target.value)}>
          <option value="all">All kinds</option>{kinds.map(value => <option key={value} value={value}>{value}</option>)}
        </NativeSelect>
        <NativeSelect aria-label="Runtime" value={runtime} onChange={event => setRuntime(event.target.value)}>
          <option value="all">All runtimes</option>{runtimes.map(value => <option key={value} value={value}>{value}</option>)}
        </NativeSelect>
        <p className="ml-auto text-xs text-muted-foreground" role="status">
          {visible.length} {visible.length === 1 ? "plugin" : "plugins"}{filtered ? ` of ${catalog.plugins.length}` : ""}
        </p>
      </div>
      {visible.length === 0 ? (
        catalog.plugins.length === 0
          ? <Empty icon={Puzzle} title="No plugins installed" description="Install a plugin with `opengtm plugin install` into a configured plugin directory, then reload this page." />
          : <Empty icon={Puzzle} title="No matching plugins" description="Change the search or filters."
            action={<Button variant="outline" size="sm" onClick={() => { setQuery(""); setKind("all"); setRuntime("all") }}>Clear filters</Button>} />
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3" aria-label="Plugins">
          {visible.map(plugin => <li key={plugin.name} className="flex"><PluginCard plugin={plugin} onRun={onRun} /></li>)}
        </ul>
      )}
    </div>
  )
}
