import { BrowserRouter, Routes, Route, NavLink, Navigate, useLocation, useNavigate, useSearchParams } from "react-router-dom"
import { lazy, Suspense, useState, useRef, useEffect, useCallback } from "react"
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
  Settings, Zap, Circle, Plus, Trash2, BarChart3, Table2, X, Activity,
  Moon, Sun, LogOut, Building2, Radar, LayoutTemplate, LoaderCircle,
} from "lucide-react"
import { useSSE, useJobs, useConversations, useLLMUsage } from "@/lib/hooks"
import { deleteConversation } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { CommandMenu } from "@/components/command-menu"
import { CollectionIntentDialog } from "@/components/collection-intent-dialog"
import { AuthProvider, useAuth } from "@/lib/auth-context"
import LoginPage from "@/pages/login"

// Pages
const ChatPage = lazy(() => import("@/pages/chat"))
const LeadsPage = lazy(() => import("@/pages/leads"))
const LeadDetailPage = lazy(() => import("@/pages/lead-detail"))
const SearchPage = lazy(() => import("@/pages/search"))
const AgentsPage = lazy(() => import("@/pages/agents"))
const TaskDetailPage = lazy(() => import("@/pages/task-detail"))
const CampaignsPage = lazy(() => import("@/pages/campaigns"))
const OutreachPage = lazy(() => import("@/pages/outreach"))
const SourcesPage = lazy(() => import("@/pages/sources"))
const SettingsPage = lazy(() => import("@/pages/settings"))
const AnalyticsPage = lazy(() => import("@/pages/analytics"))
const WorkbooksPage = lazy(() => import("@/pages/workbooks"))
const WorkbookEditorPage = lazy(() => import("@/pages/workbook-editor"))
const SignalsPage = lazy(() => import("@/pages/signals"))
const WorkspacesManagerPage = lazy(() => import("@/pages/workspaces-manager"))
const AutomationsPage = lazy(() => import("@/pages/automations"))
const WatchesPage = lazy(() => import("@/pages/watches"))
const TemplatesPage = lazy(() => import("@/pages/templates"))

const NAV_ITEMS = [
  { to: "/chat",       icon: MessageSquare, label: "Chat" },
  { to: "/leads",      icon: Users,         label: "Leads" },
  { to: "/workbooks",  icon: Table2,        label: "Workbooks" },
  { to: "/templates",  icon: LayoutTemplate, label: "Templates" },
  { to: "/search",     icon: Search,        label: "Search" },
  { to: "/agents",     icon: Bot,           label: "Tasks" },
  { to: "/outreach",   icon: Send,          label: "Outreach" },
  { to: "/automations", icon: Zap,          label: "Automations" },
  { to: "/watches",    icon: Radar,         label: "Watches" },
  { to: "/signals",    icon: Activity,      label: "Signals" },
  { to: "/sources",    icon: Database,       label: "Sources" },
  { to: "/analytics",  icon: BarChart3,     label: "Analytics" },
]

function AppSidebar() {
  const location = useLocation()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const activeChatId = searchParams.get("id")
  useSSE()
  const { data: jobs } = useJobs()
  const { data: conversations } = useConversations()
  const { user, workspaces, activeWorkspaceId, switchWorkspace, logout } = useAuth()
  const jobList = Array.isArray(jobs) ? jobs : []
  const activeJobs = jobList.filter(j => j.status === "running" || j.status === "pending").length
  const [chatSearch, setChatSearch] = useState("")
  const [isSearching, setIsSearching] = useState(false)
  const searchInputRef = useRef<HTMLInputElement>(null)

  const filteredConversations = conversations?.filter(c =>
    !chatSearch || c.title.toLowerCase().includes(chatSearch.toLowerCase())
  )

  useEffect(() => {
    if (isSearching) searchInputRef.current?.focus()
  }, [isSearching])

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
              <div className="flex aspect-square size-8 items-center justify-center">
                <img src="/opengtm-mark-v8.svg" alt="" aria-hidden="true" className="size-7 object-contain" />
              </div>
              <div className="grid flex-1 text-left text-sm leading-tight">
                <span className="truncate font-semibold">OpenGTM</span>
                <span className="truncate text-xs text-muted-foreground">GTM agents for the world</span>
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
            {isSearching ? (
              <input
                ref={searchInputRef}
                value={chatSearch}
                onChange={(e) => setChatSearch(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Escape") {
                    setChatSearch("")
                    setIsSearching(false)
                  }
                }}
                placeholder="Search chats…"
                onBlur={() => { setChatSearch(""); setIsSearching(false) }}
                className="bg-transparent border-none outline-none text-xs text-sidebar-foreground placeholder:text-muted-foreground w-full"
              />
            ) : (
              <span>Recent Chats</span>
            )}
            <div className="flex items-center gap-0.5">
              <button
                onClick={() => {
                  if (isSearching) {
                    setChatSearch("")
                    setIsSearching(false)
                  } else {
                    setIsSearching(true)
                  }
                }}
                className={`hover:text-foreground transition-opacity ${
                  isSearching ? "opacity-100" : "opacity-0 group-hover/label:opacity-100"
                }`}
                title={isSearching ? "Close search" : "Search chats"}
              >
                {isSearching ? <X className="size-3.5" /> : <Search className="size-3.5" />}
              </button>
              <button
                onClick={handleNewChat}
                className="opacity-0 group-hover/label:opacity-100 hover:text-foreground transition-opacity"
                title="New Chat"
              >
                <Plus className="size-4" />
              </button>
            </div>
          </SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {filteredConversations?.map((conv) => (
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

      <SidebarFooter className="border-t pt-2 mt-2">
        {/* Active workspace switcher */}
        {workspaces.length > 0 && (
          <div className="px-2 pb-1 group-data-[collapsible=icon]:hidden">
            <label className="flex items-center gap-1.5 text-[11px] text-muted-foreground mb-1">
              <Building2 className="size-3" /> Workspace
            </label>
            <select
              value={activeWorkspaceId ?? ""}
              onChange={(e) => { switchWorkspace(e.target.value).catch(() => {}) }}
              className="w-full rounded-md border bg-background px-2 py-1 text-xs text-foreground outline-none focus:ring-1 focus:ring-ring"
            >
              {workspaces.map((w) => (
                <option key={w.id} value={w.id}>{w.icon ? `${w.icon} ` : ""}{w.name}</option>
              ))}
            </select>
          </div>
        )}
        <div className="flex items-center justify-between px-2 py-1">
          <SidebarMenuButton
            isActive={location.pathname.startsWith("/settings")}
            tooltip="Settings"
            render={<NavLink to="/settings" />}
            className="w-auto flex-none"
          >
            <Settings />
            <span>Settings</span>
          </SidebarMenuButton>
          <SidebarMenuButton
            tooltip={user ? `Sign out (${user.username})` : "Sign out"}
            onClick={logout}
            className="w-auto flex-none"
          >
            <LogOut />
            <span>Sign out</span>
          </SidebarMenuButton>
        </div>
      </SidebarFooter>
    </Sidebar>
  )
}

function PageHeader({ title }: { title: string }) {
  const { data: usage } = useLLMUsage()
  const { connected } = useSSE()
  const activeProvider = usage?.providers?.[0]
  const [isDark, setIsDark] = useState(() => document.documentElement.classList.contains("dark"))

  const toggleTheme = useCallback(() => {
    const html = document.documentElement
    if (html.classList.contains("dark")) {
      html.classList.remove("dark")
      localStorage.setItem("theme", "light")
      setIsDark(false)
    } else {
      html.classList.add("dark")
      localStorage.setItem("theme", "dark")
      setIsDark(true)
    }
  }, [])

  return (
    <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-2 h-4" />
      <h1 className="min-w-0 truncate text-sm font-medium">{title}</h1>

      <div className="flex-1" />

      {/* LLM Usage */}
      {usage && (
        <div className="hidden items-center gap-2 md:flex">
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

      <Separator orientation="vertical" className="mx-1 hidden h-4 sm:block" />

      {/* Connection status */}
      <div
        className="flex items-center gap-1.5"
        title={connected ? "SSE connected" : "SSE disconnected"}
        aria-label={connected ? "Connected" : "Offline"}
      >
        <Circle
          className={`size-2 fill-current ${
            connected ? "text-green-500" : "text-muted-foreground"
          }`}
        />
        <span className="hidden text-[11px] text-muted-foreground sm:inline">
          {connected ? "Connected" : "Offline"}
        </span>
      </div>

      {/* Theme toggle */}
      <button
        onClick={toggleTheme}
        className="ml-1 flex items-center justify-center size-8 rounded-md hover:bg-muted transition-colors text-muted-foreground hover:text-foreground"
        title={isDark ? "Switch to light mode" : "Switch to dark mode"}
      >
        {isDark ? <Sun className="size-4" /> : <Moon className="size-4" />}
      </button>
    </header>
  )
}

function AppContent() {
  const location = useLocation()

  const getTitle = () => {
    if (location.pathname.startsWith("/chat")) return "Chat"
    if (location.pathname.startsWith("/leads")) return "Leads"
    if (location.pathname.startsWith("/workbooks")) return "Workbooks"
    if (location.pathname.startsWith("/templates")) return "Templates"
    if (location.pathname.startsWith("/search")) return "Search"
    if (location.pathname.startsWith("/agents")) return "Tasks"
    if (location.pathname.startsWith("/outreach")) return "Outreach"
    if (location.pathname.startsWith("/automations")) return "Automations"
    if (location.pathname.startsWith("/watches")) return "Watches"
    if (location.pathname.startsWith("/signals")) return "Signals"
    if (location.pathname.startsWith("/agency")) return "Agency"
    if (location.pathname.startsWith("/campaigns")) return "Campaigns"
    if (location.pathname.startsWith("/sources")) return "Sources"
    if (location.pathname.startsWith("/analytics")) return "Analytics"
    if (location.pathname.startsWith("/settings")) return "Settings"
    return "OpenGTM"
  }

  return (
    <SidebarInset className="h-screen overflow-hidden flex flex-col">
      <PageHeader title={getTitle()} />
      <div className="flex-1 min-h-0 overflow-y-auto relative">
        <Suspense fallback={<RouteSpinner />}>
          <Routes>
            <Route path="/chat/*" element={<ChatPage />} />
            <Route path="/leads/:id" element={<LeadDetailPage />} />
            <Route path="/leads" element={<LeadsPage />} />
            <Route path="/workbooks/:id" element={<div className="h-full overflow-hidden"><WorkbookEditorPage /></div>} />
            <Route path="/workbooks" element={<WorkbooksPage />} />
            <Route path="/templates" element={<TemplatesPage />} />
            <Route path="/search/*" element={<SearchPage />} />
            <Route path="/agents/:jobId" element={<TaskDetailPage />} />
            <Route path="/agents" element={<AgentsPage />} />
            <Route path="/outreach/*" element={<OutreachPage />} />
            <Route path="/automations/*" element={<AutomationsPage />} />
            <Route path="/watches/*" element={<WatchesPage />} />
            <Route path="/signals/*" element={<SignalsPage />} />
            <Route path="/agency/*" element={<WorkspacesManagerPage />} />
            <Route path="/campaigns/*" element={<CampaignsPage />} />
            <Route path="/sources/*" element={<SourcesPage />} />
            <Route path="/analytics/*" element={<AnalyticsPage />} />
            <Route path="/settings/*" element={<SettingsPage />} />
            <Route path="*" element={<Navigate to="/chat" replace />} />
          </Routes>
        </Suspense>
      </div>
    </SidebarInset>
  )
}

function RouteSpinner() {
  return (
    <div className="flex h-full min-h-48 items-center justify-center text-muted-foreground" role="status">
      <LoaderCircle className="size-5 animate-spin" />
      <span className="sr-only">Loading page…</span>
    </div>
  )
}

function FullScreenSpinner({ label }: { label?: string }) {
  return (
    <div className="flex h-screen w-full items-center justify-center bg-background">
      <div className="flex flex-col items-center gap-3 text-muted-foreground">
        <LoaderCircle className="size-5 animate-spin" />
        <span className="text-sm">{label ?? "Loading…"}</span>
      </div>
    </div>
  )
}

function Shell() {
  return (
    <>
      <div className="h-screen w-full overflow-hidden flex">
        <SidebarProvider>
          <AppSidebar />
          <AppContent />
        </SidebarProvider>
      </div>
      <CommandMenu />
      <CollectionIntentDialog />
    </>
  )
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { user, loading, activeWorkspaceId } = useAuth()
  const location = useLocation()
  if (loading) return <FullScreenSpinner />
  if (!user) return <Navigate to="/login" replace state={{ from: location.pathname }} />
  if (!activeWorkspaceId) return <FullScreenSpinner label="Loading workspace…" />
  return <>{children}</>
}

export default function App() {
  return (
    <TooltipProvider>
      <BrowserRouter>
        <AuthProvider>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/*" element={<RequireAuth><Shell /></RequireAuth>} />
          </Routes>
          <Toaster />
        </AuthProvider>
      </BrowserRouter>
    </TooltipProvider>
  )
}
