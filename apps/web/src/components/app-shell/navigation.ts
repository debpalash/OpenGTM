import {
  Activity, BarChart3, Bot, Building2, Database, LayoutTemplate, ListFilter,
  MessageSquare, Radar, Search, Send, Settings, Table2, Users, Zap,
} from "lucide-react"

export const NAVIGATION_GROUPS = [
  { label: "Workspace", items: [
    { to: "/chat", icon: MessageSquare, label: "Chat" },
    { to: "/workbooks", icon: Table2, label: "Workbooks" },
    { to: "/leads", icon: Users, label: "Leads" },
    { to: "/audiences", icon: ListFilter, label: "Audiences" },
    { to: "/search", icon: Search, label: "Search" },
  ] },
  { label: "Execution", items: [
    { to: "/agents", icon: Bot, label: "Tasks" },
    { to: "/automations", icon: Zap, label: "Automations" },
    { to: "/watches", icon: Radar, label: "Watches" },
    { to: "/signals", icon: Activity, label: "Signals" },
    { to: "/outreach", icon: Send, label: "Outreach" },
    { to: "/campaigns", icon: Send, label: "Campaigns" },
  ] },
  { label: "Resources", items: [
    { to: "/sources", icon: Database, label: "Sources" },
    { to: "/templates", icon: LayoutTemplate, label: "Templates" },
    { to: "/analytics", icon: BarChart3, label: "Analytics" },
  ] },
]

export const UTILITY_NAVIGATION = [
  { to: "/agency", icon: Building2, label: "Manage workspaces" },
  { to: "/settings", icon: Settings, label: "Settings" },
]
export const ALL_NAVIGATION = [...NAVIGATION_GROUPS.flatMap(group => group.items), ...UTILITY_NAVIGATION]
export const isNavigationActive = (pathname: string, destination: string) =>
  pathname === destination || pathname.startsWith(`${destination}/`)
export const getPageTitle = (pathname: string) =>
  ALL_NAVIGATION.find(item => isNavigationActive(pathname, item.to))?.label ?? "OpenGTM"

export const OPEN_COMMAND_MENU_EVENT = "opengtm:open-command-menu"
