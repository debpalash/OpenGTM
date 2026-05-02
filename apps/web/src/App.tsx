import { BrowserRouter, Routes, Route, NavLink, Navigate } from "react-router-dom"
import { Toaster } from "sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { Search, Users, Globe, Download, Briefcase, LayoutDashboard, Settings2 } from "lucide-react"

// Pages
import LeadsPage from "@/pages/leads"
import SearchPage from "@/pages/search"
import IntelPage from "@/pages/intel"
import ScraperPage from "@/pages/scraper"
import DownloadsPage from "@/pages/downloads"
import SettingsPage from "@/pages/settings"
import PipelinePage from "@/pages/pipeline"

const NAV_ITEMS = [
  { to: "/leads", icon: Users, label: "Leads" },
  { to: "/search", icon: Search, label: "Search" },
  { to: "/scraper", icon: Globe, label: "Scraper" },
  { to: "/intel", icon: Briefcase, label: "Intel" },
  { to: "/downloads", icon: Download, label: "Downloads" },
]

function AppShell() {
  return (
    <div className="h-screen flex bg-background text-foreground">
      {/* ─── Sidebar ─── */}
      <nav className="w-14 border-r border-border flex flex-col items-center py-3 gap-1 shrink-0 bg-card/50">
        {/* Logo */}
        <div className="w-8 h-8 rounded-lg bg-primary/10 flex items-center justify-center mb-3">
          <LayoutDashboard size={16} className="text-primary" />
        </div>

        {NAV_ITEMS.map(item => (
          <NavLink
            key={item.to}
            to={item.to}
            className={({ isActive }) =>
              `w-10 h-10 rounded-lg flex flex-col items-center justify-center gap-0.5 transition-all group relative ${
                isActive
                  ? "bg-primary/15 text-primary"
                  : "text-muted-foreground hover:text-foreground hover:bg-card"
              }`
            }
          >
            <item.icon size={16} />
            <span className="text-[8px] font-medium leading-none">{item.label}</span>
          </NavLink>
        ))}

        {/* Spacer */}
        <div className="flex-1" />

        {/* Settings at bottom */}
        <NavLink
          to="/settings"
          className={({ isActive }) =>
            `w-10 h-10 rounded-lg flex flex-col items-center justify-center gap-0.5 transition-all mb-1 ${
              isActive
                ? "bg-primary/15 text-primary"
                : "text-muted-foreground hover:text-foreground hover:bg-card"
            }`
          }
        >
          <Settings2 size={16} />
          <span className="text-[8px] font-medium leading-none">Settings</span>
        </NavLink>
      </nav>

      {/* ─── Main Content ─── */}
      <main className="flex-1 flex flex-col min-w-0 min-h-0">
        <Routes>
          <Route path="/leads/*" element={<LeadsPage />} />
          <Route path="/search" element={<SearchPage />} />
          <Route path="/scraper" element={<ScraperPage />} />
          <Route path="/intel" element={<IntelPage />} />
          <Route path="/downloads" element={<DownloadsPage />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="/pipeline/:jobId" element={<PipelinePage />} />
          <Route path="*" element={<Navigate to="/leads" replace />} />
        </Routes>
      </main>

      <Toaster position="bottom-right" theme="dark" />
    </div>
  )
}

export default function App() {
  return (
    <TooltipProvider>
      <BrowserRouter>
        <AppShell />
      </BrowserRouter>
    </TooltipProvider>
  )
}
