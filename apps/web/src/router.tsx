// Typed, code-based route tree for the web app (TanStack Router).
//
// Code-based routes keep every path in one reviewable file and avoid a
// generated route tree and its build plugin; each page is a lazily loaded
// chunk. Paths, query parameters and redirects match the previous
// react-router table.

import { lazy, Suspense } from "react"
import {
  type AnyRoute, createRootRouteWithContext, createRoute, createRouter, lazyRouteComponent, Outlet, redirect,
} from "@tanstack/react-router"
import type { QueryClient } from "@tanstack/react-query"
import * as z from "zod/mini"
import { AuthenticatedLayout, RouteSpinner } from "@/components/app-shell/app-layout"
import { leadQueryOptions } from "@/lib/hooks"
import { queryClient } from "@/lib/query-client"
import { parseSearch, stringifySearch } from "@/lib/router-search"

export interface RouterContext {
  queryClient: QueryClient
  /** True once the session and active workspace are settled; set by <App>. */
  authReady: boolean
}

declare module "@tanstack/react-router" {
  interface Register { router: typeof router }
  interface HistoryState {
    /** Where /login returns to after signing in (path + query + fragment). */
    from?: string
  }
}

// Development builds only; production replaces the import with a no-op.
// Development builds only; production replaces the import with a no-op.
const RouterDevtools = import.meta.env.DEV
  ? lazy(() => import("@tanstack/react-router-devtools").then(module => ({ default: module.TanStackRouterDevtools })))
  : () => null

// ── Search schemas ──
// Every value is a string (see lib/router-search.ts); pages keep their own
// defaults and parsing. These run in the entry bundle, so they use zod/mini;
// the full zod build still loads only with the pages that need it.
const optionalString = () => z.optional(z.string())
const chatSearch = z.object({ id: optionalString(), draft: optionalString() })
const outreachSearch = z.object({ draft: optionalString() })
const watchesSearch = z.object({ id: optionalString(), create: optionalString(), target: optionalString() })
const workbooksSearch = z.object({ q: optionalString(), status: optionalString(), sort: optionalString(), page: optionalString() })

// ── Routes ──
const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: () => <>
    <Outlet />
    <Suspense fallback={null}><RouterDevtools position="bottom-right" /></Suspense>
  </>,
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: lazyRouteComponent(() => import("@/pages/login")),
})

/** Pathless layout: auth gate + app shell for every signed-in page. */
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "_app",
  component: AuthenticatedLayout,
})

// react-router `/section/*` routes matched the section and any sub-path; the
// pages ignore the remainder. A child splat keeps those URLs on the page.
const splat = <TParent extends AnyRoute>(parent: TParent) => createRoute({ getParentRoute: () => parent, path: "$" })

const chatRoute = createRoute({
  getParentRoute: () => appRoute, path: "chat", validateSearch: chatSearch,
  component: lazyRouteComponent(() => import("@/pages/chat")),
})
const leadsRoute = createRoute({
  getParentRoute: () => appRoute, path: "leads",
  component: lazyRouteComponent(() => import("@/pages/leads")),
})
const leadDetailRoute = createRoute({
  getParentRoute: () => appRoute, path: "leads/$id",
  // Start the lead request alongside the page chunk on in-app navigation.
  // Prefetch never throws; the page's own query owns loading and errors.
  loader: ({ context, params }) => {
    const id = Number(params.id)
    if (context.authReady && id) void context.queryClient.prefetchQuery(leadQueryOptions(id))
  },
  component: lazyRouteComponent(() => import("@/pages/lead-detail")),
})
const audiencesRoute = createRoute({
  getParentRoute: () => appRoute, path: "audiences",
  component: lazyRouteComponent(() => import("@/pages/audiences")),
})
const workbookEditorRoute = createRoute({
  getParentRoute: () => appRoute, path: "workbooks/$id",
  component: lazyRouteComponent(() => import("@/pages/workbook-editor").then(({ default: WorkbookEditorPage }) => ({
    default: () => <div className="h-full overflow-hidden"><WorkbookEditorPage /></div>,
  }))),
})
const workbooksRoute = createRoute({
  getParentRoute: () => appRoute, path: "workbooks", validateSearch: workbooksSearch,
  component: lazyRouteComponent(() => import("@/pages/workbooks")),
})
const templatesRoute = createRoute({
  getParentRoute: () => appRoute, path: "templates",
  component: lazyRouteComponent(() => import("@/pages/templates")),
})
const searchRoute = createRoute({
  getParentRoute: () => appRoute, path: "search",
  component: lazyRouteComponent(() => import("@/pages/search")),
})
const taskDetailRoute = createRoute({
  getParentRoute: () => appRoute, path: "agents/$jobId",
  component: lazyRouteComponent(() => import("@/pages/task-detail")),
})
const agentsRoute = createRoute({
  getParentRoute: () => appRoute, path: "agents",
  component: lazyRouteComponent(() => import("@/pages/agents")),
})
const outreachRoute = createRoute({
  getParentRoute: () => appRoute, path: "outreach", validateSearch: outreachSearch,
  component: lazyRouteComponent(() => import("@/pages/outreach")),
})
const automationsRoute = createRoute({
  getParentRoute: () => appRoute, path: "automations",
  component: lazyRouteComponent(() => import("@/pages/automations")),
})
const watchesRoute = createRoute({
  getParentRoute: () => appRoute, path: "watches", validateSearch: watchesSearch,
  component: lazyRouteComponent(() => import("@/pages/watches")),
})
const signalsRoute = createRoute({
  getParentRoute: () => appRoute, path: "signals",
  component: lazyRouteComponent(() => import("@/pages/signals")),
})
const agencyRoute = createRoute({
  getParentRoute: () => appRoute, path: "agency",
  component: lazyRouteComponent(() => import("@/pages/workspaces-manager")),
})
const campaignsRoute = createRoute({
  getParentRoute: () => appRoute, path: "campaigns",
  component: lazyRouteComponent(() => import("@/pages/campaigns")),
})
const sourcesRoute = createRoute({
  getParentRoute: () => appRoute, path: "sources",
  component: lazyRouteComponent(() => import("@/pages/sources")),
})
const analyticsRoute = createRoute({
  getParentRoute: () => appRoute, path: "analytics",
  component: lazyRouteComponent(() => import("@/pages/analytics")),
})
const notificationsRoute = createRoute({
  getParentRoute: () => appRoute, path: "notifications",
  component: lazyRouteComponent(() => import("@/pages/notifications")),
})
const settingsRoute = createRoute({
  getParentRoute: () => appRoute, path: "settings",
  component: lazyRouteComponent(() => import("@/pages/settings")),
})
/** Any other signed-in path, including `/`, goes to the chat home. */
const fallbackRoute = createRoute({
  getParentRoute: () => appRoute, path: "$",
  beforeLoad: () => { throw redirect({ to: "/chat", replace: true }) },
})

export const routeTree = rootRoute.addChildren([
  loginRoute,
  appRoute.addChildren([
    chatRoute.addChildren([splat(chatRoute)]),
    leadDetailRoute,
    leadsRoute,
    audiencesRoute,
    workbookEditorRoute,
    workbooksRoute,
    templatesRoute,
    searchRoute.addChildren([splat(searchRoute)]),
    taskDetailRoute,
    agentsRoute,
    outreachRoute.addChildren([splat(outreachRoute)]),
    automationsRoute.addChildren([splat(automationsRoute)]),
    watchesRoute.addChildren([splat(watchesRoute)]),
    signalsRoute.addChildren([splat(signalsRoute)]),
    agencyRoute.addChildren([splat(agencyRoute)]),
    campaignsRoute.addChildren([splat(campaignsRoute)]),
    sourcesRoute.addChildren([splat(sourcesRoute)]),
    analyticsRoute.addChildren([splat(analyticsRoute)]),
    notificationsRoute,
    settingsRoute.addChildren([splat(settingsRoute)]),
    fallbackRoute,
  ]),
])

export const router = createRouter({
  routeTree,
  context: { queryClient, authReady: false },
  parseSearch,
  stringifySearch,
  defaultPendingComponent: RouteSpinner,
})
