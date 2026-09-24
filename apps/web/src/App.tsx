import { BrowserRouter, Routes, Route, Navigate, Link, useLocation } from "react-router-dom"
import { Fragment, lazy, Suspense, useState } from "react"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { SidebarProvider, SidebarInset, SidebarTrigger } from "@/components/ui/sidebar"
import { AppSidebar } from "@/components/app-shell/app-sidebar"
import { getPageTitle } from "@/components/app-shell/navigation"
import { ArrowUpRight, BellRing, Circle, LoaderCircle } from "lucide-react"
import { useSSE, useLLMUsage } from "@/lib/hooks"
import { CommandMenu } from "@/components/command-menu"
import { KeyboardLayer } from "@/components/keyboard/keyboard-layer"
import { QuickLookProvider } from "@/components/quick-look/quick-look"
import { CollectionIntentDialog } from "@/components/collection-intent-dialog"
import { AuthProvider, useAuth } from "@/lib/auth-context"
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@/components/ui/popover"
import { NotificationRow } from "@/components/notifications/notification-row"
import { useNotifications } from "@/lib/notifications"
import LoginPage from "@/pages/login"

// Pages
const ChatPage = lazy(() => import("@/pages/chat"))
const LeadsPage = lazy(() => import("@/pages/leads"))
const AudiencesPage = lazy(() => import("@/pages/audiences"))
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
const NotificationsPage = lazy(() => import("@/pages/notifications"))


function NotificationBell() {
  const [open, setOpen] = useState(false)
  const query = useNotifications()
  const items = query.data ?? []
  const critical = items.filter(item => item.severity === "critical").length
  const urgent = items.length - critical
  const label = query.isPending ? "Loading notifications" : query.isError
    ? "Notifications unavailable" : `Notifications: ${critical} critical, ${urgent} urgent`

  return <Popover open={open} onOpenChange={setOpen}>
    <PopoverTrigger render={<button type="button" className="gtm-notification-trigger" aria-label={label} />}>
      <span className="gtm-notification-symbol"><BellRing className="size-[18px]" aria-hidden="true" /></span>
      <span className="gtm-notification-count" data-severity={critical ? "critical" : urgent ? "urgent" : undefined}>
        {query.isPending ? "…" : query.isError ? "!" : items.length > 99 ? "99+" : items.length}
      </span>
    </PopoverTrigger>
    <PopoverContent align="end" sideOffset={10} className="w-[min(410px,calc(100vw-24px))] gap-0 overflow-hidden p-0">
      <div className="border-b border-border/70 px-4 py-3">
        <div className="flex items-center justify-between gap-3">
          <div>
            <PopoverTitle className="text-sm font-semibold">Attention center</PopoverTitle>
            <p className="text-[11px] text-muted-foreground">Recent signals and work needing your attention</p>
          </div>
          <BellRing className="size-4 text-muted-foreground" aria-hidden="true" />
        </div>
        {!query.isPending && !query.isError && <div className="mt-3 flex gap-2 text-[11px] font-medium">
          <span className="rounded-full bg-rose-500/10 px-2 py-1 text-rose-600 dark:text-rose-300">{critical} critical</span>
          <span className="rounded-full bg-amber-500/10 px-2 py-1 text-amber-700 dark:text-amber-300">{urgent} urgent</span>
        </div>}
      </div>
      <div className="max-h-[min(420px,calc(100vh-180px))] space-y-1 overflow-y-auto p-2">
        {query.isPending && <p role="status" className="p-4 text-center text-xs text-muted-foreground">Loading notifications…</p>}
        {query.isError && <div role="alert" className="space-y-2 p-4 text-center text-xs">
          <p>Could not load notifications.</p>
          <button type="button" className="font-medium text-primary underline-offset-2 hover:underline" onClick={() => void query.refetch()}>Try again</button>
        </div>}
        {!query.isPending && !query.isError && items.length === 0 && <p className="p-5 text-center text-xs text-muted-foreground">All clear. No recent issues need attention.</p>}
        {!query.isPending && !query.isError && items.slice(0, 4).map(item =>
          <NotificationRow key={item.id} notification={item} compact onNavigate={() => setOpen(false)} />)}
      </div>
      <Link to="/notifications" onClick={() => setOpen(false)}
        className="flex items-center justify-between border-t border-border/70 px-4 py-3 text-xs font-semibold text-primary transition-colors hover:bg-muted/50">
        View all notifications <ArrowUpRight className="size-4" aria-hidden="true" />
      </Link>
    </PopoverContent>
  </Popover>
}

function PageHeader({ title }: { title: string }) {
  const { data: usage } = useLLMUsage()
  const { connected } = useSSE()
  const activeProvider = usage?.providers?.[0]

  return (
    <header className="gtm-material-toolbar gtm-topbar sticky top-0 z-20 flex h-[var(--gtm-toolbar-height)] shrink-0 items-center gap-3 border-b border-[var(--t-border-color-medium)] px-4">
      <SidebarTrigger className="-ml-1 text-muted-foreground" />
      <h1 className="min-w-0 truncate text-base font-semibold">{title}</h1>

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

      <span aria-hidden="true" className="mx-1 hidden h-4 w-px self-center bg-border sm:block" />

      {/* Connection status */}
      <div
        className="gtm-topbar-chip"
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

      <NotificationBell />
    </header>
  )
}

function AppContent() {
  const location = useLocation()
  // The chat home paints a sky; the toolbar continues it.
  const isChatHome = location.pathname.replace(/\/$/, "") === "/chat" && !new URLSearchParams(location.search).get("id")


  return (
    <SidebarInset className="gtm-launch h-screen overflow-hidden flex flex-col" data-sky={isChatHome || undefined}>
      <PageHeader title={getPageTitle(location.pathname)} />
      <div className="flex-1 min-h-0 overflow-y-auto relative">
        {/* Keyed by top-level section: switching sections plays the page
            transition; navigation within a section does not remount. */}
        <div key={location.pathname.split("/")[1] || "root"} className="gtm-page-enter h-full">
        <Suspense fallback={<RouteSpinner />}>
          <Routes>
            <Route path="/chat/*" element={<ChatPage />} />
            <Route path="/leads/:id" element={<LeadDetailPage />} />
            <Route path="/leads" element={<LeadsPage />} />
            <Route path="/audiences" element={<AudiencesPage />} />
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
            <Route path="/notifications" element={<NotificationsPage />} />
            <Route path="/settings/*" element={<SettingsPage />} />
            <Route path="*" element={<Navigate to="/chat" replace />} />
          </Routes>
        </Suspense>
        </div>
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
    <QuickLookProvider>
      <div className="h-screen w-full overflow-hidden flex">
        <SidebarProvider>
          <AppSidebar />
          <AppContent />
          <KeyboardLayer />
        </SidebarProvider>
      </div>
      <CommandMenu />
      <CollectionIntentDialog />
    </QuickLookProvider>
  )
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { user, loading, activeWorkspaceId } = useAuth()
  const location = useLocation()
  if (loading) return <FullScreenSpinner />
  if (!user) return <Navigate to="/login" replace state={{ from: location.pathname + location.search + location.hash }} />
  if (!activeWorkspaceId) return <FullScreenSpinner label="Loading workspace…" />
  return <Fragment key={activeWorkspaceId}>{children}</Fragment>
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
