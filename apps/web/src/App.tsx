import { BrowserRouter, Routes, Route, NavLink, Navigate, useLocation, useNavigate, useSearchParams } from "react-router-dom"
import { Toaster } from "sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupLabel,
  SidebarGroupContent, SidebarHeader, SidebarMenu, SidebarMenuButton,
  SidebarMenuItem, SidebarProvider, SidebarInset, SidebarTrigger,
} from "@/components/ui/sidebar"
import { Separator } from "@/components/ui/separator"
import { Badge } from "@/components/ui/badge"
import {
  MessageSquare, Users, Search, Bot, Send, Database,
  Settings, Zap, Circle, Plus, Trash2, BarChart3,
} from "lucide-react"
import { useSSE, useJobs, useConversations, useLLMUsage } from "@/lib/hooks"
import { deleteConversation } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { CommandMenu } from "@/components/command-menu"

// Pages
import ChatPage from "@/pages/chat"
import LeadsPage from "@/pages/leads"
import LeadDetailPage from "@/pages/lead-detail"
import SearchPage from "@/pages/search"
import AgentsPage from "@/pages/agents"
import TaskDetailPage from "@/pages/task-detail"
import CampaignsPage from "@/pages/campaigns"
import SourcesPage from "@/pages/sources"
import SettingsPage from "@/pages/settings"
import AnalyticsPage from "@/pages/analytics"

const NAV_ITEMS = [
  { to: "/chat",      icon: MessageSquare, label: "Chat" },
  { to: "/leads",     icon: Users,         label: "Leads" },
  { to: "/search",    icon: Search,        label: "Search" },
  { to: "/agents",    icon: Bot,           label: "Tasks" },
  { to: "/campaigns", icon: Send,          label: "Campaigns" },
  { to: "/sources",   icon: Database,      label: "Sources" },
  { to: "/analytics", icon: BarChart3,      label: "Analytics" },
]

function AppSidebar() {
  const location = useLocation()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const activeChatId = searchParams.get("id")
  const { connected } = useSSE()
  const { data: jobs } = useJobs()
  const { data: conversations } = useConversations()
  const activeJobs = jobs?.filter(j => j.status === "running" || j.status === "pending").length ?? 0

  const handleNewChat = (e: React.MouseEvent) => {
    e.preventDefault()
    navigate("/chat")
  }

  const handleDeleteChat = async (id: string, e: React.MouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
    if (!confirm("Delete this conversation?")) return
    try {
      await deleteConversation(id)
      queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
      if (activeChatId === id) {
        navigate("/chat")
      }
    } catch {
      // ignore
    }
  }

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              size="lg"
              render={<NavLink to="/chat" />}
            >
              <div className="flex aspect-square size-8 items-center justify-center rounded-lg overflow-hidden bg-background">
                <img src="/logo.svg" alt="Yupcha" className="size-7" />
              </div>
              <div className="grid flex-1 text-left text-sm leading-tight">
                <span className="truncate font-semibold">Yupcha Sales</span>
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
        
        {/* Chat History Group */}
        <SidebarGroup className="pt-2">
          <SidebarGroupLabel className="flex justify-between items-center group/label">
            Recent Chats
            <button
              onClick={handleNewChat}
              className="opacity-0 group-hover/label:opacity-100 hover:text-foreground transition-opacity"
              title="New Chat"
            >
              <Plus className="size-4" />
            </button>
          </SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {conversations?.map((conv) => (
                <SidebarMenuItem key={conv.id}>
                  <SidebarMenuButton
                    isActive={location.pathname === "/chat" && activeChatId === conv.id}
                    tooltip={conv.title}
                    render={<NavLink to={`/chat?id=${conv.id}`} />}
                    className="group/item"
                  >
                    <MessageSquare className="size-4 opacity-70" />
                    <span className="truncate">{conv.title}</span>
                    <button
                      onClick={(e) => handleDeleteChat(conv.id, e)}
                      className="ml-auto opacity-0 group-hover/item:opacity-100 hover:text-destructive"
                      title="Delete"
                    >
                      <Trash2 className="size-3" />
                    </button>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
              {!conversations?.length && (
                <div className="px-4 py-2 text-xs text-muted-foreground">
                  No recent chats
                </div>
              )}
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
  const { data: usage } = useLLMUsage()
  const activeProvider = usage?.providers?.[0]

  return (
    <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-2 h-4" />
      <h1 className="text-sm font-medium">{title}</h1>

      <div className="flex-1" />

      {/* LLM Usage */}
      {usage && (
        <div className="flex items-center gap-2">
          {activeProvider ? (
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className="font-medium text-foreground capitalize">{activeProvider.provider}</span>
              <span className="tabular-nums">{activeProvider.calls}/{activeProvider.daily_limit}</span>
              <div className="w-12 h-1.5 rounded-full bg-muted overflow-hidden">
                <div
                  className={`h-full rounded-full transition-all ${activeProvider.pct > 80 ? 'bg-destructive' : activeProvider.pct > 50 ? 'bg-yellow-500' : 'bg-primary'}`}
                  style={{ width: `${activeProvider.pct}%` }}
                />
              </div>
            </div>
          ) : null}
          {usage.today_calls > 0 && (
            <span className="text-[10px] text-muted-foreground tabular-nums">
              {usage.today_calls} calls · {(usage.today_tokens / 1000).toFixed(1)}k tok
            </span>
          )}
        </div>
      )}
    </header>
  )
}

function AppContent() {
  const location = useLocation()

  const getTitle = () => {
    if (location.pathname.startsWith("/chat")) return "Chat"
    if (location.pathname.startsWith("/leads")) return "Leads"
    if (location.pathname.startsWith("/search")) return "Search"
    if (location.pathname.startsWith("/agents")) return "Tasks"
    if (location.pathname.startsWith("/campaigns")) return "Campaigns"
    if (location.pathname.startsWith("/sources")) return "Sources"
    if (location.pathname.startsWith("/analytics")) return "Analytics"
    if (location.pathname.startsWith("/settings")) return "Settings"
    return "Yupcha Sales"
  }

  return (
    <SidebarInset className="h-screen overflow-hidden flex flex-col">
      <PageHeader title={getTitle()} />
      <div className="flex-1 min-h-0 overflow-y-auto relative">
        <Routes>
          <Route path="/chat/*" element={<ChatPage />} />
          <Route path="/leads/:id" element={<LeadDetailPage />} />
          <Route path="/leads" element={<LeadsPage />} />
          <Route path="/search/*" element={<SearchPage />} />
          <Route path="/agents/:jobId" element={<TaskDetailPage />} />
          <Route path="/agents" element={<AgentsPage />} />
          <Route path="/campaigns/*" element={<CampaignsPage />} />
          <Route path="/sources/*" element={<SourcesPage />} />
          <Route path="/analytics/*" element={<AnalyticsPage />} />
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
        <div className="h-screen w-full overflow-hidden flex">
          <SidebarProvider>
            <AppSidebar />
            <AppContent />
          </SidebarProvider>
        </div>
        <CommandMenu />
        <Toaster />
      </BrowserRouter>
    </TooltipProvider>
  )
}
