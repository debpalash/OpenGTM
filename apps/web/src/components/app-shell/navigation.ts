import {
  Activity, Archive, BarChart3, BellRing, Bot, Building2, Database, LayoutTemplate, ListFilter,
  House, Megaphone, MessageSquare, Radar, Search, Send, Settings, Table2, Users, Zap,
  type LucideIcon,
} from "lucide-react"

export interface NavigationPage { to: string; key: string; icon: LucideIcon; label: string }
export interface NavigationArea {
  id: string
  label: string
  icon: LucideIcon
  /** top: pinned entries above the areas; main: collapsible areas; footer: admin. */
  placement: "top" | "main" | "footer"
  pages: NavigationPage[]
}

/** The single source of navigation: eight entries, six of them task areas. */
export const NAVIGATION_AREAS: NavigationArea[] = [
  { id: "home", label: "Home", icon: House, placement: "top", pages: [
    { to: "/overview", key: "d", icon: House, label: "Overview" },
  ] },
  { id: "assistant", label: "Assistant", icon: MessageSquare, placement: "top", pages: [
    { to: "/chat", key: "h", icon: MessageSquare, label: "Assistant" },
  ] },
  { id: "find", label: "Find", icon: Search, placement: "main", pages: [
    { to: "/search", key: "f", icon: Search, label: "Search" },
    { to: "/sources", key: "r", icon: Database, label: "Data sources" },
    { to: "/signals", key: "i", icon: Activity, label: "Signals" },
    { to: "/watches", key: "v", icon: Radar, label: "Watches" },
  ] },
  { id: "records", label: "Records", icon: Building2, placement: "main", pages: [
    { to: "/companies", key: "b", icon: Building2, label: "Companies" },
    { to: "/people", key: "e", icon: Users, label: "People" },
    { to: "/audiences", key: "a", icon: ListFilter, label: "Audiences" },
    { to: "/leads", key: "l", icon: Archive, label: "Legacy data" },
  ] },
  { id: "enrich", label: "Enrich", icon: Table2, placement: "main", pages: [
    { to: "/workbooks", key: "w", icon: Table2, label: "Workbooks" },
    { to: "/templates", key: "m", icon: LayoutTemplate, label: "Templates" },
    { to: "/agents", key: "t", icon: Bot, label: "Tasks" },
  ] },
  { id: "engage", label: "Engage", icon: Send, placement: "main", pages: [
    { to: "/outreach", key: "o", icon: Send, label: "Outreach" },
    { to: "/campaigns", key: "p", icon: Megaphone, label: "Campaigns" },
    { to: "/automations", key: "u", icon: Zap, label: "Automations" },
  ] },
  { id: "measure", label: "Measure", icon: BarChart3, placement: "main", pages: [
    { to: "/analytics", key: "y", icon: BarChart3, label: "Analytics" },
  ] },
  { id: "admin", label: "Admin", icon: Settings, placement: "footer", pages: [
    { to: "/settings", key: "s", icon: Settings, label: "Settings" },
    { to: "/agency", key: "k", icon: Building2, label: "Workspaces" },
  ] },
]

/** Notifications is a utility control (the bell), not part of an area. */
export const UTILITY_NAVIGATION: NavigationPage[] = [
  { to: "/notifications", key: "n", icon: BellRing, label: "Notifications" },
]
/** Routes that exist but deliberately belong to no area. */
export const AREALESS_ROUTES = ["/login", "/setup", "/notifications"]

export const ALL_NAVIGATION: NavigationPage[] = [...NAVIGATION_AREAS.flatMap(area => area.pages), ...UTILITY_NAVIGATION]
export const isNavigationActive = (pathname: string, destination: string) =>
  pathname === destination || pathname.startsWith(`${destination}/`)
export const getNavigationArea = (pathname: string) =>
  NAVIGATION_AREAS.find(area => area.pages.some(page => isNavigationActive(pathname, page.to)))
export const getPageTitle = (pathname: string) =>
  ALL_NAVIGATION.find(item => isNavigationActive(pathname, item.to))?.label ?? "OpenGTM"
/** First breadcrumb: the area name. Home and Assistant are not areas, so a page
 * under them (or under no area) falls back to "Workspace". */
export const getBreadcrumbRoot = (pathname: string) => {
  const area = getNavigationArea(pathname)
  return area && area.placement !== "top" ? area.label : "Workspace"
}

export const OPEN_COMMAND_MENU_EVENT = "opengtm:open-command-menu"
export const OPEN_SHORTCUTS_EVENT = "opengtm:open-shortcuts"
