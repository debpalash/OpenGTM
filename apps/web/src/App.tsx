import { BrowserRouter, Routes, Route, NavLink, Navigate, useLocation } from "react-router-dom"
import { Toaster } from "sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup,
  SidebarGroupContent, SidebarHeader, SidebarMenu, SidebarMenuButton,
  SidebarMenuItem, SidebarProvider, SidebarInset, SidebarTrigger,
} from "@/components/ui/sidebar"
import { Separator } from "@/components/ui/separator"
import { Badge } from "@/components/ui/badge"
import {
  MessageSquare, Users, Search, Bot, Send, Database,
  Settings, Zap, Circle,
} from "lucide-react"
import { useSSE, useJobs } from "@/lib/hooks"
import { CommandMenu } from "@/components/command-menu"

// Pages
import ChatPage from "@/pages/chat"
import LeadsPage from "@/pages/leads"
import LeadDetailPage from "@/pages/lead-detail"
import SearchPage from "@/pages/search"
import AgentsPage from "@/pages/agents"
import CampaignsPage from "@/pages/campaigns"
import SourcesPage from "@/pages/sources"
import SettingsPage from "@/pages/settings"

const NAV_ITEMS = [
  { to: "/chat",      icon: MessageSquare, label: "Chat" },
  { to: "/leads",     icon: Users,         label: "Leads" },
  { to: "/search",    icon: Search,        label: "Search" },
  { to: "/agents",    icon: Bot,           label: "Agents" },
  { to: "/campaigns", icon: Send,          label: "Campaigns" },
  { to: "/sources",   icon: Database,      label: "Sources" },
]

function AppSidebar() {
  const location = useLocation()
  const { connected } = useSSE()
  const { data: jobs } = useJobs()
  const activeJobs = jobs?.filter(j => j.status === "running" || j.status === "pending").length ?? 0

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              size="lg"
              render={<NavLink to="/chat" />}
            >
              <div className="flex aspect-square size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
                <Zap className="size-4" />
              </div>
              <div className="grid flex-1 text-left text-sm leading-tight">
                <span className="truncate font-semibold">LeadEngine</span>
                <span className="truncate text-xs text-muted-foreground">AI-powered leads</span>
              </div>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupContent>
            <SidebarMenu>
              {NAV_ITEMS.map((item) => (
                <SidebarMenuItem key={item.to}>
                  <SidebarMenuButton
                    isActive={location.pathname.startsWith(item.to)}
                    tooltip={item.label}
                    render={<NavLink to={item.to} />}
                  >
                    <item.icon />
                    <span>{item.label}</span>
                    {item.to === "/agents" && activeJobs > 0 && (
                      <Badge variant="secondary" className="ml-auto text-xs">
                        {activeJobs}
                      </Badge>
                    )}
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              isActive={location.pathname.startsWith("/settings")}
              tooltip="Settings"
              render={<NavLink to="/settings" />}
            >
              <Settings />
              <span>Settings</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton tooltip="Status">
              <Circle
                className={`size-2 fill-current ${connected ? "text-green-500" : "text-muted-foreground"}`}
              />
              <span className="text-xs text-muted-foreground">
                {connected ? "Connected" : "Disconnected"}
              </span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  )
}

function PageHeader({ title }: { title: string }) {
  return (
    <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-2 h-4" />
      <h1 className="text-sm font-medium">{title}</h1>
    </header>
  )
}

function AppContent() {
  const location = useLocation()

  const getTitle = () => {
    if (location.pathname.startsWith("/chat")) return "Chat"
    if (location.pathname.startsWith("/leads")) return "Leads"
    if (location.pathname.startsWith("/search")) return "Search"
    if (location.pathname.startsWith("/agents")) return "Agents"
    if (location.pathname.startsWith("/campaigns")) return "Campaigns"
    if (location.pathname.startsWith("/sources")) return "Sources"
    if (location.pathname.startsWith("/settings")) return "Settings"
    return "LeadEngine"
  }

  return (
    <SidebarInset>
      <PageHeader title={getTitle()} />
      <div className="flex-1 overflow-auto">
        <Routes>
          <Route path="/chat/*" element={<ChatPage />} />
          <Route path="/leads/:id" element={<LeadDetailPage />} />
          <Route path="/leads" element={<LeadsPage />} />
          <Route path="/search/*" element={<SearchPage />} />
          <Route path="/agents/*" element={<AgentsPage />} />
          <Route path="/campaigns/*" element={<CampaignsPage />} />
          <Route path="/sources/*" element={<SourcesPage />} />
          <Route path="/settings/*" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/chat" replace />} />
        </Routes>
      </div>
    </SidebarInset>
  )
}

export default function App() {
  return (
    <TooltipProvider>
      <BrowserRouter>
        <SidebarProvider>
          <AppSidebar />
          <AppContent />
        </SidebarProvider>
        <CommandMenu />
        <Toaster />
      </BrowserRouter>
    </TooltipProvider>
  )
}
