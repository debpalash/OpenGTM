import { useState } from "react"
import { NavLink, useLocation, useNavigate, useSearchParams } from "react-router-dom"
import { ChevronDown, LogOut, MessageSquare, Plus, Search, Trash2 } from "lucide-react"
import { toast } from "sonner"
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupLabel,
  SidebarGroupContent, SidebarHeader, SidebarMenu, SidebarMenuButton,
  SidebarMenuItem, SidebarMenuAction, useSidebar,
} from "@/components/ui/sidebar"
import { Badge } from "@/components/ui/badge"
import { useConversations, useJobs } from "@/lib/hooks"
import { useAuth } from "@/lib/auth-context"
import { deleteConversation } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { NAVIGATION_GROUPS, UTILITY_NAVIGATION, isNavigationActive, OPEN_COMMAND_MENU_EVENT } from "./navigation"
import { NativeSelect } from "@/components/ui/native-select"
import { Button } from "@/components/ui/button"

export function AppSidebar() {
  const { pathname } = useLocation()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const { isMobile, setOpenMobile } = useSidebar()
  const { data: jobs } = useJobs()
  const { data: conversations } = useConversations()
  const { user, workspaces, activeWorkspaceId, switchWorkspace, logout } = useAuth()
  const [switching, setSwitching] = useState(false)
  const [chatSearch, setChatSearch] = useState("")
  const [historyOpen, setHistoryOpen] = useState(false)
  const activeJobs = (Array.isArray(jobs) ? jobs : []).filter(job => job.status === "running" || job.status === "pending").length
  const filtered = (conversations ?? []).filter(chat => chat.title.toLowerCase().includes(chatSearch.toLowerCase()))
  const closeMobile = () => { if (isMobile) setOpenMobile(false) }

  async function changeWorkspace(id: string) {
    setSwitching(true)
    try { await switchWorkspace(id); navigate("/chat"); closeMobile() }
    catch (error) { toast.error(error instanceof Error ? error.message : "Could not switch workspace. Try again.") }
    finally { setSwitching(false) }
  }

  async function removeChat(id: string) {
    if (!confirm("Delete this conversation?")) return
    try {
      await deleteConversation(id)
      await queryClient.invalidateQueries({ queryKey: queryKeys.conversations.all })
      if (pathname === "/chat" && params.get("id") === id) navigate("/chat")
    } catch { toast.error("Could not delete the conversation. Try again.") }
  }

  return <Sidebar collapsible="icon">
    <SidebarHeader className="gap-3 border-b px-3 py-3">
      <SidebarMenuButton render={<NavLink to="/chat" />} onClick={closeMobile} tooltip="OpenGTM">
        <img src="/opengtm-mark-v8.svg" alt="" className="size-5" />
        <span className="font-semibold">OpenGTM</span>
      </SidebarMenuButton>
      <div className="group-data-[collapsible=icon]:hidden">
        <label htmlFor="active-workspace" className="sr-only">Active workspace</label>
        <NativeSelect id="active-workspace" value={activeWorkspaceId ?? ""} disabled={switching} aria-busy={switching}
          onChange={event => void changeWorkspace(event.target.value)} className="w-full">
          {workspaces.map(workspace => <option key={workspace.id} value={workspace.id}>{workspace.name}</option>)}
        </NativeSelect>
      </div>
      <SidebarMenuButton tooltip="Find anything" onClick={() => { closeMobile(); window.dispatchEvent(new Event(OPEN_COMMAND_MENU_EVENT)) }}>
        <Search aria-hidden="true" /><span>Find anything</span>
      </SidebarMenuButton>
    </SidebarHeader>
    <SidebarContent>
      <nav aria-label="Primary navigation">
        {NAVIGATION_GROUPS.map(group => <SidebarGroup key={group.label} className="py-1">
          <SidebarGroupLabel>{group.label}</SidebarGroupLabel>
          <SidebarGroupContent><SidebarMenu>
            {group.items.map(item => <SidebarMenuItem key={item.to}>
              <SidebarMenuButton isActive={isNavigationActive(pathname, item.to)} tooltip={item.label}
                render={<NavLink to={item.to} />} onClick={closeMobile}>
                <item.icon aria-hidden="true" /><span>{item.label}</span>
                {item.to === "/agents" && activeJobs > 0 && <Badge variant="secondary" className="ml-auto tabular-nums" aria-label={`${activeJobs} active tasks`}>{activeJobs}</Badge>}
              </SidebarMenuButton>
            </SidebarMenuItem>)}
          </SidebarMenu></SidebarGroupContent>
        </SidebarGroup>)}
      </nav>
      <SidebarGroup className="group-data-[collapsible=icon]:hidden">
        <Button variant="ghost" size="sm" className="w-full justify-between px-2 text-xs"
          aria-expanded={historyOpen} aria-controls="recent-chat-list" onClick={() => setHistoryOpen(!historyOpen)}>
          Recent chats<ChevronDown className={`size-3 transition-transform ${historyOpen ? "rotate-180" : ""}`} aria-hidden="true" />
        </Button>
        {historyOpen && <SidebarGroupContent id="recent-chat-list">
          <div className="flex items-center gap-2 p-2">
            <input aria-label="Search recent chats" value={chatSearch} onChange={event => setChatSearch(event.target.value)} placeholder="Search chats"
              className="h-8 min-w-0 flex-1 rounded-md border border-border bg-background px-2 text-sm outline-none focus-visible:outline-2 focus-visible:outline-ring sm:h-7" />
            <Button variant="ghost" size="icon-sm" aria-label="New chat" onClick={() => { navigate("/chat"); closeMobile() }}><Plus /></Button>
          </div>
          <SidebarMenu>{filtered.map(chat => <SidebarMenuItem key={chat.id}>
            <SidebarMenuButton isActive={pathname === "/chat" && params.get("id") === chat.id} tooltip={chat.title}
              render={<NavLink to={`/chat?id=${encodeURIComponent(chat.id)}`} />} onClick={closeMobile} className="pr-9">
              <MessageSquare aria-hidden="true" /><span>{chat.title}</span>
            </SidebarMenuButton>
            <SidebarMenuAction aria-label={`Delete ${chat.title}`} onClick={() => void removeChat(chat.id)}><Trash2 aria-hidden="true" /></SidebarMenuAction>
          </SidebarMenuItem>)}</SidebarMenu>
          {!filtered.length && <p className="px-2 py-3 text-xs text-muted-foreground">{chatSearch ? "No matching chats" : "No recent chats"}</p>}
        </SidebarGroupContent>}
      </SidebarGroup>
    </SidebarContent>
    <SidebarFooter className="border-t">
      <SidebarMenu>{UTILITY_NAVIGATION.map(item => <SidebarMenuItem key={item.to}>
        <SidebarMenuButton render={<NavLink to={item.to} />} onClick={closeMobile} tooltip={item.label} isActive={isNavigationActive(pathname, item.to)}>
          <item.icon aria-hidden="true" /><span>{item.label}</span>
        </SidebarMenuButton>
      </SidebarMenuItem>)}
        <SidebarMenuItem><SidebarMenuButton tooltip={user ? `Sign out (${user.username})` : "Sign out"} onClick={logout}><LogOut aria-hidden="true" /><span>Sign out</span></SidebarMenuButton></SidebarMenuItem>
      </SidebarMenu>
    </SidebarFooter>
  </Sidebar>
}
