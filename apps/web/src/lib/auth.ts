// Auth token + active-workspace persistence and a global fetch interceptor.
//
// The backend (see core/tenancy.py) now requires `Authorization: Bearer <jwt>`
// on every business endpoint and an `X-Workspace-Id` header to pick a workspace.
// Rather than editing ~60 scattered fetch() call sites, we wrap window.fetch once
// so every same-origin /api request (and /auth/me) carries the right headers and
// 401s funnel through a single logout handler.

const TOKEN_KEY = "yupcha_token"
const WS_KEY = "yupcha_workspace_id"

let unauthorizedHandler: (() => void) | null = null

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

export function getActiveWorkspace(): string | null {
  return localStorage.getItem(WS_KEY)
}

export function setActiveWorkspace(id: string | null): void {
  if (id) localStorage.setItem(WS_KEY, id)
  else localStorage.removeItem(WS_KEY)
}

/** Register the callback fired when any authed request returns 401. */
export function onUnauthorized(fn: () => void): void {
  unauthorizedHandler = fn
}

/** Build the query suffix used to authenticate EventSource / WebSocket
 *  connections, which cannot send an Authorization header. */
export function authQuery(): string {
  const params = new URLSearchParams()
  const token = getToken()
  if (token) params.set("token", token)
  const ws = getActiveWorkspace()
  if (ws) params.set("workspace_id", ws)
  const s = params.toString()
  return s ? `?${s}` : ""
}

function urlOf(input: RequestInfo | URL): string {
  if (typeof input === "string") return input
  if (input instanceof URL) return input.toString()
  return input.url
}

/** A request that should carry our auth headers: our own API, plus /auth/me. */
function needsAuth(url: string): boolean {
  return url.startsWith("/api/") || url.startsWith("/auth/me")
}

let installed = false

/** Wrap window.fetch exactly once. Idempotent. Call before the app renders. */
export function installFetchInterceptor(): void {
  if (installed) return
  installed = true
  const orig = window.fetch.bind(window)

  window.fetch = async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = urlOf(input)
    if (!needsAuth(url)) return orig(input as RequestInfo, init)

    const headers = new Headers(
      init.headers ?? (input instanceof Request ? input.headers : undefined),
    )
    const token = getToken()
    if (token && !headers.has("Authorization")) {
      headers.set("Authorization", `Bearer ${token}`)
    }
    const ws = getActiveWorkspace()
    if (ws && !headers.has("X-Workspace-Id")) {
      headers.set("X-Workspace-Id", ws)
    }

    const res = await orig(input as RequestInfo, { ...init, headers })
    if (res.status === 401) {
      clearToken()
      unauthorizedHandler?.()
    }
    return res
  }
}
