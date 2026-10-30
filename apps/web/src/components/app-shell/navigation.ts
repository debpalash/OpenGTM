import {
  Activity, BarChart3, BellRing, Bot, Building2, Database, LayoutTemplate, ListFilter,
  House, Megaphone, MessageSquare, Radar, Search, Send, Settings, Table2, Users, Zap,
} from "lucide-react"

export const NAVIGATION_GROUPS = [
  { label: "Workspace", items: [
    { to: "/overview", key: "d", icon: House, label: "Overview" },
    { to: "/companies", key: "b", icon: Building2, label: "Companies" },
    { to: "/people", key: "e", icon: Users, label: "People" },
    { to: "/workbooks", key: "w", icon: Table2, label: "Workbooks" },
    { to: "/audiences", key: "a", icon: ListFilter, label: "Audiences" },
    { to: "/signals", key: "i", icon: Activity, label: "Signals" },
    { to: "/outreach", key: "o", icon: Send, label: "Outreach" },
    { to: "/chat", key: "h", icon: MessageSquare, label: "Assistant" },
  ] },
  { label: "Operations", items: [
    { to: "/agents", key: "t", icon: Bot, label: "Tasks" },
    { to: "/automations", key: "u", icon: Zap, label: "Automations" },
    { to: "/watches", key: "v", icon: Radar, label: "Watches" },
    { to: "/campaigns", key: "p", icon: Megaphone, label: "Campaigns" },
  ] },
  { label: "Resources", items: [
    { to: "/sources", key: "r", icon: Database, label: "Data sources" },
    { to: "/templates", key: "m", icon: LayoutTemplate, label: "Templates" },
    { to: "/analytics", key: "y", icon: BarChart3, label: "Analytics" },
  ] },
]

export const UTILITY_NAVIGATION = [
  { to: "/search", key: "f", icon: Search, label: "Search" },
  { to: "/leads", key: "l", icon: Users, label: "Legacy data" },
  { to: "/agency", key: "k", icon: Building2, label: "Manage workspaces" },
  { to: "/notifications", key: "n", icon: BellRing, label: "Notifications" },
  { to: "/settings", key: "s", icon: Settings, label: "Settings" },
]
export const ALL_NAVIGATION = [...NAVIGATION_GROUPS.flatMap(group => group.items), ...UTILITY_NAVIGATION]
export const isNavigationActive = (pathname: string, destination: string) =>
  pathname === destination || pathname.startsWith(`${destination}/`)
export const getPageTitle = (pathname: string) =>
  ALL_NAVIGATION.find(item => isNavigationActive(pathname, item.to))?.label ?? "OpenGTM"

export const OPEN_COMMAND_MENU_EVENT = "opengtm:open-command-menu"
export const OPEN_SHORTCUTS_EVENT = "opengtm:open-shortcuts"
