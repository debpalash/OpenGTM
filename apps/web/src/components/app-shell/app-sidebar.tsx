import { useState } from "react"
import { NavLink, useLocation, useNavigate, useSearchParams } from "react-router-dom"
import {
  Building2, Check, ChevronRight, ChevronsUpDown, Keyboard, LogOut, MoreHorizontal,
  Search, Settings, SquarePen, Trash2,
} from "lucide-react"
import { toast } from "sonner"
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupLabel,
  SidebarGroupContent, SidebarHeader, SidebarInput, SidebarMenu, SidebarMenuAction,
  SidebarMenuBadge, SidebarMenuButton, SidebarMenuItem, SidebarRail, useSidebar,
} from "@/components/ui/sidebar"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuGroup, DropdownMenuItem, DropdownMenuLabel,
  DropdownMenuSeparator, DropdownMenuShortcut, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Kbd } from "@/components/ui/kbd"
import { useConversations, useJobs } from "@/lib/hooks"
import { useAuth } from "@/lib/auth-context"
import { deleteConversation } from "@/lib/api"
import { queryClient, queryKeys } from "@/lib/query-client"
import { NAVIGATION_GROUPS, isNavigationActive, OPEN_COMMAND_MENU_EVENT, OPEN_SHORTCUTS_EVENT } from "./navigation"
import "./shell.css"

const RECENT_LIMIT = 8
const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform)
const initials = (name: string) => name.split(/[\s._-]+/).filter(Boolean).slice(0, 2).map(part => part[0]!.toUpperCase()).join("") || "?"

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
  const [historyOpen, setHistoryOpen] = useState(true)
  const activeJobs = (Array.isArray(jobs) ? jobs : []).filter(job => job.status === "running" || job.status === "pending").length
  const matching = (conversations ?? []).filter(chat => chat.title.toLowerCase().includes(chatSearch.toLowerCase()))
  const recent = chatSearch ? matching : matching.slice(0, RECENT_LIMIT)
  const activeWorkspace = workspaces.find(workspace => workspace.id === activeWorkspaceId)
  const closeMobile = () => { if (isMobile) setOpenMobile(false) }
  const go = (to: string) => { navigate(to); closeMobile() }

  async function changeWorkspace(id: string) {
    if (id === activeWorkspaceId) return
    setSwitching(true)
    try { await switchWorkspace(id); go("/chat") }
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
    <SidebarHeader>
      {/* Workspace switcher (shadcn "team switcher") */}
      <SidebarMenu><SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger aria-label="Switch workspace" disabled={switching} aria-busy={switching}
            render={<SidebarMenuButton size="lg" className="data-popup-open:bg-sidebar-accent" />}>
            <span className="gtm-shell-logo"><img src="/opengtm-mark-v8.svg" alt="" /></span>
            <span className="grid min-w-0 flex-1 text-left leading-tight">
              <span className="truncate text-sm font-semibold">OpenGTM</span>
              <span className="truncate text-xs text-sidebar-foreground/60">{activeWorkspace?.name ?? "No workspace"}</span>
            </span>
            <ChevronsUpDown className="ml-auto size-4 opacity-50" aria-hidden="true" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" side={isMobile ? "bottom" : "right"} sideOffset={6} className="min-w-60">
            <DropdownMenuGroup>
              <DropdownMenuLabel>Workspaces</DropdownMenuLabel>
              {workspaces.map(workspace => (
                <DropdownMenuItem key={workspace.id} onClick={() => void changeWorkspace(workspace.id)}>
                  <span className="flex size-6 items-center justify-center rounded-md border border-border text-[11px] font-semibold">{initials(workspace.name)}</span>
                  <span className="truncate">{workspace.name}</span>
                  {workspace.id === activeWorkspaceId && <Check className="ml-auto size-4" aria-label="Active" />}
                </DropdownMenuItem>
              ))}
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={() => go("/agency")}><Building2 aria-hidden="true" />Manage workspaces</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem></SidebarMenu>

      <SidebarMenu className="gap-1.5">
        <SidebarMenuItem>
          <SidebarMenuButton variant="outline" tooltip="Find anything" className="gtm-shell-search text-sidebar-foreground/70"
            onClick={() => { closeMobile(); window.dispatchEvent(new Event(OPEN_COMMAND_MENU_EVENT)) }}>
            <Search aria-hidden="true" /><span>Find anything</span>
            <Kbd className="ml-auto group-data-[collapsible=icon]:hidden">{isMac ? "⌘" : "Ctrl"} K</Kbd>
          </SidebarMenuButton>
        </SidebarMenuItem>
        <SidebarMenuItem>
          <SidebarMenuButton tooltip="New chat" render={<NavLink to="/chat" end />} isActive={false} onClick={closeMobile}>
            <SquarePen aria-hidden="true" /><span>New chat</span>
          </SidebarMenuButton>
        </SidebarMenuItem>
      </SidebarMenu>
    </SidebarHeader>

    <SidebarContent>
      <nav aria-label="Primary navigation">
        {NAVIGATION_GROUPS.map(group => <SidebarGroup key={group.label}>
          <SidebarGroupLabel>{group.label}</SidebarGroupLabel>
          <SidebarGroupContent><SidebarMenu>
            {group.items.map(item => <SidebarMenuItem key={item.to}>
              <SidebarMenuButton isActive={isNavigationActive(pathname, item.to)} tooltip={item.label}
                render={<NavLink to={item.to} />} onClick={closeMobile}>
                <item.icon aria-hidden="true" /><span>{item.label}</span>
              </SidebarMenuButton>
              {item.to === "/agents" && activeJobs > 0 && <SidebarMenuBadge aria-label={`${activeJobs} active tasks`}>{activeJobs}</SidebarMenuBadge>}
            </SidebarMenuItem>)}
          </SidebarMenu></SidebarGroupContent>
        </SidebarGroup>)}
      </nav>

      <SidebarGroup className="group-data-[collapsible=icon]:hidden">
        <SidebarGroupLabel render={<button type="button" aria-expanded={historyOpen} aria-controls="recent-chat-list" onClick={() => setHistoryOpen(!historyOpen)} />}
          className="cursor-pointer hover:text-sidebar-foreground">
          Recent chats
          <ChevronRight className={`ml-auto transition-transform ${historyOpen ? "rotate-90" : ""}`} aria-hidden="true" />
        </SidebarGroupLabel>
        {historyOpen && <SidebarGroupContent id="recent-chat-list">
          {(conversations?.length ?? 0) > RECENT_LIMIT || chatSearch ? (
            <SidebarInput aria-label="Search recent chats" value={chatSearch} onChange={event => setChatSearch(event.target.value)}
              placeholder="Search chats" className="mb-1 h-7" />
          ) : null}
          <SidebarMenu>{recent.map(chat => <SidebarMenuItem key={chat.id}>
            <SidebarMenuButton size="sm" isActive={pathname === "/chat" && params.get("id") === chat.id}
              render={<NavLink to={`/chat?id=${encodeURIComponent(chat.id)}`} />} onClick={closeMobile}
              className="text-sidebar-foreground/80 data-active:text-sidebar-foreground">
              <span>{chat.title || "Untitled chat"}</span>
            </SidebarMenuButton>
            <DropdownMenu>
              <DropdownMenuTrigger render={<SidebarMenuAction showOnHover aria-label={`Actions for ${chat.title}`} />}>
                <MoreHorizontal aria-hidden="true" />
              </DropdownMenuTrigger>
              <DropdownMenuContent side="right" align="start">
                <DropdownMenuItem variant="destructive" onClick={() => void removeChat(chat.id)}>
                  <Trash2 aria-hidden="true" />Delete
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </SidebarMenuItem>)}</SidebarMenu>
          {!recent.length && <p className="px-2 py-2 text-xs text-sidebar-foreground/60">{chatSearch ? "No matching chats" : "No recent chats"}</p>}
        </SidebarGroupContent>}
      </SidebarGroup>
    </SidebarContent>

    {/* Account menu (shadcn "nav user") */}
    <SidebarFooter>
      <SidebarMenu><SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger aria-label="Account menu" render={<SidebarMenuButton size="lg" className="data-popup-open:bg-sidebar-accent" />}>
            <Avatar className="size-8 rounded-lg">
              <AvatarFallback className="rounded-lg bg-sidebar-primary text-xs font-semibold text-sidebar-primary-foreground">{initials(user?.username ?? "")}</AvatarFallback>
            </Avatar>
            <span className="grid min-w-0 flex-1 text-left leading-tight">
              <span className="truncate text-sm font-medium">{user?.username ?? "Signed out"}</span>
              <span className="truncate text-xs capitalize text-sidebar-foreground/60">{user?.role ?? ""}</span>
            </span>
            <ChevronsUpDown className="ml-auto size-4 opacity-50" aria-hidden="true" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" side={isMobile ? "top" : "right"} sideOffset={6} className="min-w-56">
            <DropdownMenuGroup>
              <DropdownMenuItem onClick={() => go("/settings")}><Settings aria-hidden="true" />Settings</DropdownMenuItem>
              <DropdownMenuItem onClick={() => go("/agency")}><Building2 aria-hidden="true" />Manage workspaces</DropdownMenuItem>
              <DropdownMenuItem onClick={() => window.dispatchEvent(new Event(OPEN_SHORTCUTS_EVENT))}>
                <Keyboard aria-hidden="true" />Keyboard shortcuts<DropdownMenuShortcut>?</DropdownMenuShortcut>
              </DropdownMenuItem>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={logout}><LogOut aria-hidden="true" />Sign out</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem></SidebarMenu>
    </SidebarFooter>
    <SidebarRail />
  </Sidebar>
}
