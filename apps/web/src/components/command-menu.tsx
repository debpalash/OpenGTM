import { useEffect, useState, useMemo } from "react"
import { useNavigate } from "react-router-dom"
import {
  CommandDialog, CommandEmpty, CommandGroup, CommandInput,
  CommandItem, CommandList, CommandSeparator,
} from "@/components/ui/command"
import {
  MessageSquare, Users, Search, Bot, Send, Database,
  Settings, Download, Moon, Sun, Plus,
} from "lucide-react"
import { useLeads } from "@/lib/hooks"

const NAV_ITEMS = [
  { label: "Chat",      to: "/chat",      icon: MessageSquare },
  { label: "Leads",     to: "/leads",     icon: Users },
  { label: "Search",    to: "/search",    icon: Search },
  { label: "Tasks",     to: "/agents",    icon: Bot },
  { label: "Campaigns", to: "/campaigns", icon: Send },
  { label: "Sources",   to: "/sources",   icon: Database },
  { label: "Settings",  to: "/settings",  icon: Settings },
]

export function CommandMenu() {
  const [open, setOpen] = useState(false)
  const navigate = useNavigate()
  const { data: leads } = useLeads({ limit: "100" })

  // Global ⌘K / Ctrl+K listener
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault()
        setOpen((prev) => !prev)
      }
    }
    window.addEventListener("keydown", handler)
    return () => window.removeEventListener("keydown", handler)
  }, [])

  const go = (path: string) => {
    navigate(path)
    setOpen(false)
  }

  const isDark = document.documentElement.classList.contains("dark")

  const toggleTheme = () => {
    const html = document.documentElement
    if (html.classList.contains("dark")) {
      html.classList.remove("dark")
      localStorage.setItem("theme", "light")
    } else {
      html.classList.add("dark")
      localStorage.setItem("theme", "dark")
    }
    setOpen(false)
  }

  const leadItems = useMemo(() => {
    return (leads ?? []).slice(0, 20).map((lead) => ({
      id: lead.id,
      label: lead.company,
      sub: lead.city || lead.specialization || "",
    }))
  }, [leads])

  return (
    <CommandDialog open={open} onOpenChange={setOpen}>
      <CommandInput placeholder="Type a command or search..." />
      <CommandList>
        <CommandEmpty>No results found.</CommandEmpty>

        <CommandGroup heading="Navigate">
          {NAV_ITEMS.map((item) => (
            <CommandItem
              key={item.to}
              onSelect={() => go(item.to)}
            >
              <item.icon className="mr-2 size-4" />
              {item.label}
            </CommandItem>
          ))}
        </CommandGroup>

        <CommandSeparator />

        <CommandGroup heading="Actions">
          <CommandItem onSelect={() => { go("/chat"); setOpen(false) }}>
            <Plus className="mr-2 size-4" />
            New collection
          </CommandItem>
          <CommandItem onSelect={() => { window.open("/api/export/csv", "_blank"); setOpen(false) }}>
            <Download className="mr-2 size-4" />
            Export CSV
          </CommandItem>
          <CommandItem onSelect={toggleTheme}>
            {isDark ? <Sun className="mr-2 size-4" /> : <Moon className="mr-2 size-4" />}
            {isDark ? "Switch to light mode" : "Switch to dark mode"}
          </CommandItem>
        </CommandGroup>

        {leadItems.length > 0 && (
          <>
            <CommandSeparator />
            <CommandGroup heading="Leads">
              {leadItems.map((lead) => (
                <CommandItem
                  key={lead.id}
                  onSelect={() => go(`/leads?search=${encodeURIComponent(lead.label)}`)}
                >
                  <Users className="mr-2 size-4" />
                  <span>{lead.label}</span>
                  {lead.sub && (
                    <span className="ml-auto text-xs text-muted-foreground">{lead.sub}</span>
                  )}
                </CommandItem>
              ))}
            </CommandGroup>
          </>
        )}
      </CommandList>
    </CommandDialog>
  )
}
