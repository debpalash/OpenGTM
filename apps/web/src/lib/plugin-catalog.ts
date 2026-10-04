// Pure catalog helpers (search, filters, trust hints) for the Plugins page.

import type { PluginInfo } from "./plugin-api"

export const pluginTitle = (plugin: Pick<PluginInfo, "name" | "display_name">) => plugin.display_name || plugin.name

/** Patterns that admit far more than one site deserve a visible warning. */
export function broadNetworkPattern(pattern: string): string | null {
  if (/^https?:\/\/\*(:\*)?\/?$/.test(pattern)) return "Any host on the internet (still subject to the SSRF guard)"
  if (/^http:\/\//.test(pattern)) return "Plain HTTP"
  return null
}

/** Every search term must appear in the name, title, description, author or tags. */
export function filterPlugins(plugins: PluginInfo[], query: string, kind: string, runtime: string): PluginInfo[] {
  const terms = query.trim().toLowerCase().split(/\s+/).filter(Boolean)
  return plugins.filter(plugin => {
    if (kind !== "all" && plugin.kind !== kind) return false
    if (runtime !== "all" && plugin.runtime !== runtime) return false
    const haystack = [plugin.name, plugin.display_name, plugin.description, plugin.author, ...plugin.tags].join(" ").toLowerCase()
    return terms.every(term => haystack.includes(term))
  })
}
