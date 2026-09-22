import { useMemo } from "react"
import { useNavigate } from "react-router-dom"
import {
  CommandDialog, CommandEmpty, CommandGroup, CommandInput,
  CommandItem, CommandList, CommandSeparator,
} from "@/components/ui/command"
import { Users, Download, Moon, Sun, Plus } from "lucide-react"
import { useLeads } from "@/lib/hooks"
import { useTheme } from "@/design-system/theme/use-theme"
import { ALL_NAVIGATION } from "@/components/app-shell/navigation"

export function CommandMenuContent({ open, setOpen }: { open: boolean; setOpen: (open: boolean) => void }) {
  const navigate = useNavigate()
  const { data: leads } = useLeads({ limit: "100" }, { enabled: open })

  const go = (path: string) => {
    navigate(path)
    setOpen(false)
  }

  const { colorScheme, setPreference } = useTheme()
  const isDark = colorScheme === "dark"

  const toggleTheme = () => {
    setPreference(isDark ? "light" : "dark")
    setOpen(false)
  }

  const leadItems = useMemo(() => {
    return (Array.isArray(leads) ? leads : []).slice(0, 20).map((lead) => ({
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
          {ALL_NAVIGATION.map((item) => (
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
