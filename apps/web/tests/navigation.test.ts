import { describe, expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import {
  ALL_NAVIGATION, AREALESS_ROUTES, NAVIGATION_AREAS, getBreadcrumbRoot, getNavigationArea, getPageTitle,
  isNavigationActive,
} from "../src/components/app-shell/navigation"

const appSource = readFileSync(new URL("../src/App.tsx", import.meta.url), "utf8")
// <Route path="/x/:id" /> and <Route path="/x/*" /> reduce to their static prefix; "*" is the catch-all.
const declaredRoutes = [...new Set([
  ...[...appSource.matchAll(/<Route path="([^"]+)"/g)].map(match => match[1]),
  ...[...appSource.matchAll(/^\s*\{ path: "([^"*]+)",/gm)].map(match => `/${match[1]}`),
].filter(path => path !== "*").map(path => path.replace(/\/(:[^/]+|\*)$/, "")))]

describe("shared application navigation", () => {
  test("retains every top-level product route once", () => {
    const routes = ALL_NAVIGATION.map(item => item.to)
    expect(new Set(routes).size).toBe(routes.length)
    expect([...routes].sort()).toEqual([
      "/overview", "/companies", "/people", "/chat", "/leads", "/audiences", "/workbooks", "/templates", "/search",
      "/agents", "/outreach", "/automations", "/watches", "/signals", "/sources",
      "/analytics", "/agency", "/campaigns", "/notifications", "/settings",
    ].sort())
  })
  test("keeps every single-key shortcut unique and unchanged", () => {
    const keys = ALL_NAVIGATION.map(item => item.key)
    expect(new Set(keys).size).toBe(keys.length)
    expect(Object.fromEntries(ALL_NAVIGATION.map(item => [item.key, item.to]))).toMatchObject({
      d: "/overview", b: "/companies", e: "/people", w: "/workbooks", a: "/audiences", i: "/signals", o: "/outreach",
      h: "/chat", t: "/agents", u: "/automations", v: "/watches", p: "/campaigns", r: "/sources", m: "/templates",
      y: "/analytics", f: "/search", l: "/leads", k: "/agency", n: "/notifications", s: "/settings",
    })
  })
  test("organises pages into the six task areas plus Home and Assistant", () => {
    expect(NAVIGATION_AREAS.filter(area => area.placement !== "top").map(area => area.label))
      .toEqual(["Find", "Records", "Enrich", "Engage", "Measure", "Admin"])
    expect(NAVIGATION_AREAS.filter(area => area.placement === "top").map(area => area.label)).toEqual(["Home", "Assistant"])
    const pages = (id: string) => NAVIGATION_AREAS.find(area => area.id === id)?.pages.map(page => page.to)
    expect(pages("find")).toEqual(["/search", "/sources", "/signals", "/watches"])
    expect(pages("records")).toEqual(["/companies", "/people", "/audiences", "/leads"])
    expect(pages("enrich")).toEqual(["/workbooks", "/templates", "/agents"])
    expect(pages("engage")).toEqual(["/outreach", "/campaigns", "/automations"])
    expect(pages("measure")).toEqual(["/analytics"])
    expect(pages("admin")).toEqual(["/settings", "/agency"])
  })
  test("maps nested routes to their area", () => {
    expect(getNavigationArea("/workbooks/wb-1")?.id).toBe("enrich")
    expect(getNavigationArea("/leads/contact-123")?.id).toBe("records")
    expect(getNavigationArea("/agents/job-123")?.id).toBe("enrich")
    expect(getNavigationArea("/agency/team-123")?.id).toBe("admin")
    expect(getNavigationArea("/chat")?.id).toBe("assistant")
    expect(getNavigationArea("/leads-other")).toBeUndefined()
    expect(getNavigationArea("/notifications")).toBeUndefined()
  })
  test("every route declared in App.tsx maps to exactly one area or is explicitly area-less", () => {
    expect(declaredRoutes.length).toBeGreaterThan(15)
    for (const route of declaredRoutes) {
      const owners = NAVIGATION_AREAS.filter(area => area.pages.some(page => isNavigationActive(route, page.to)))
      const expected = AREALESS_ROUTES.includes(route) ? 0 : 1
      expect({ route, owners: owners.length }).toEqual({ route, owners: expected })
    }
    expect(declaredRoutes).toContain("/login")
    expect(declaredRoutes).toContain("/setup")
  })
  test("titles pages and names the area for the breadcrumb", () => {
    expect(getPageTitle("/agents/job-123")).toBe("Tasks")
    expect(getPageTitle("/agency/team-123")).toBe("Workspaces")
    expect(getBreadcrumbRoot("/workbooks/wb-1")).toBe("Enrich")
    expect(getBreadcrumbRoot("/overview")).toBe("Workspace")
    expect(getBreadcrumbRoot("/notifications")).toBe("Workspace")
  })
  test("matches route boundaries rather than similarly prefixed paths", () => {
    expect(isNavigationActive("/leads/contact-123", "/leads")).toBe(true)
    expect(isNavigationActive("/leads-other", "/leads")).toBe(false)
    expect(getPageTitle("/unknown")).toBe("OpenGTM")
  })
})
