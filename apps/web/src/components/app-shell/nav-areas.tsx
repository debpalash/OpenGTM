import { useCallback, useEffect, useMemo, useState } from "react"
import { NavLink } from "react-router-dom"
import { ChevronRight } from "lucide-react"
import {
  SidebarGroup, SidebarMenu, SidebarMenuBadge, SidebarMenuButton, SidebarMenuItem,
  SidebarMenuSub, SidebarMenuSubButton, SidebarMenuSubItem,
} from "@/components/ui/sidebar"
import { getNavigationArea, isNavigationActive, type NavigationArea } from "./navigation"

const STORAGE_KEY = "opengtm.nav.areas"

function readOverrides(): Record<string, boolean> {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "{}")
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {}
    return Object.fromEntries(Object.entries(parsed).filter(([, value]) => typeof value === "boolean"))
  } catch { return {} }
}

/**
 * Which areas are expanded. The area holding the current route is open by
 * default and every other area is closed; what a person opens or closes is
 * remembered per browser. Navigating into an area reopens it.
 */
export function useAreaDisclosure(pathname: string) {
  const currentId = getNavigationArea(pathname)?.id
  const [overrides, setOverrides] = useState<Record<string, boolean>>(readOverrides)

  const save = useCallback((next: Record<string, boolean>) => {
    try { localStorage.setItem(STORAGE_KEY, JSON.stringify(next)) } catch { /* storage unavailable: keep in memory */ }
  }, [])

  useEffect(() => {
    if (!currentId) return
    setOverrides(previous => {
      if (previous[currentId] !== false) return previous
      const next = { ...previous }
      delete next[currentId]
      save(next)
      return next
    })
  }, [currentId, save])

  const isExpanded = useCallback((id: string) => overrides[id] ?? id === currentId, [overrides, currentId])
  const toggle = useCallback((id: string) => {
    setOverrides(previous => {
      const open = previous[id] ?? id === currentId
      const next = { ...previous, [id]: !open }
      save(next)
      return next
    })
  }, [currentId, save])
  return useMemo(() => ({ currentId, isExpanded, toggle }), [currentId, isExpanded, toggle])
}

interface NavAreasProps {
  areas: NavigationArea[]
  pathname: string
  /** Desktop rail: show every page as an icon with a tooltip, no disclosure. */
  iconOnly: boolean
  disclosure: ReturnType<typeof useAreaDisclosure>
  activeTasks: number
  onNavigate: () => void
}

export function NavAreas({ areas, pathname, iconOnly, disclosure, activeTasks, onNavigate }: NavAreasProps) {
  return <>{areas.map(area => {
    const current = disclosure.currentId === area.id
    const taskBadge = (to: string) => to === "/agents" && activeTasks > 0

    if (iconOnly) {
      return <SidebarGroup key={area.id}><SidebarMenu>
        {area.pages.map(page => <SidebarMenuItem key={page.to}>
          <SidebarMenuButton isActive={isNavigationActive(pathname, page.to)} tooltip={`${area.label}: ${page.label}`}
            render={<NavLink to={page.to} />} onClick={onNavigate}>
            <page.icon aria-hidden="true" /><span>{page.label}</span>
          </SidebarMenuButton>
          {taskBadge(page.to) && <SidebarMenuBadge aria-label={`${activeTasks} active tasks`}>{activeTasks}</SidebarMenuBadge>}
        </SidebarMenuItem>)}
      </SidebarMenu></SidebarGroup>
    }

    // An area with a single page is a plain link, not a disclosure.
    if (area.pages.length === 1) {
      const page = area.pages[0]!
      return <SidebarGroup key={area.id}><SidebarMenu><SidebarMenuItem>
        <SidebarMenuButton isActive={isNavigationActive(pathname, page.to)} render={<NavLink to={page.to} />}
          className="gtm-nav-area-link" onClick={onNavigate}>
          <area.icon aria-hidden="true" /><span>{area.label}</span>
        </SidebarMenuButton>
      </SidebarMenuItem></SidebarMenu></SidebarGroup>
    }

    const open = disclosure.isExpanded(area.id)
    const panelId = `nav-area-${area.id}`
    return <SidebarGroup key={area.id}><SidebarMenu><SidebarMenuItem>
      <SidebarMenuButton type="button" className="gtm-nav-area-toggle" data-current={current || undefined}
        aria-expanded={open} aria-controls={panelId} aria-current={current ? "true" : undefined}
        onClick={() => disclosure.toggle(area.id)}>
        <area.icon aria-hidden="true" /><span>{area.label}</span>
        {!open && area.pages.some(page => taskBadge(page.to)) && <span className="ml-auto text-xs tabular-nums">
          {activeTasks}<span className="sr-only"> active tasks</span>
        </span>}
        <ChevronRight aria-hidden="true" className={`gtm-nav-chevron size-3.5 ${open ? "rotate-90" : ""} ${open || !area.pages.some(page => taskBadge(page.to)) ? "ml-auto" : ""}`} />
      </SidebarMenuButton>
      <SidebarMenuSub id={panelId} hidden={!open} className="gtm-nav-area-panel">
        {area.pages.map(page => <SidebarMenuSubItem key={page.to}>
          <SidebarMenuSubButton isActive={isNavigationActive(pathname, page.to)} render={<NavLink to={page.to} />}
            onClick={onNavigate}>
            <page.icon aria-hidden="true" /><span>{page.label}</span>
            {taskBadge(page.to) && <span className="ml-auto text-xs tabular-nums">
              {activeTasks}<span className="sr-only"> active tasks</span>
            </span>}
          </SidebarMenuSubButton>
        </SidebarMenuSubItem>)}
      </SidebarMenuSub>
    </SidebarMenuItem></SidebarMenu></SidebarGroup>
  })}</>
}
