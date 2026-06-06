import { createContext, useContext, useState, useEffect, useCallback, type ReactNode } from "react"
import {
  getToken, setToken, clearToken,
  getActiveWorkspace, setActiveWorkspace, onUnauthorized,
} from "./auth"

export interface AuthUser {
  id: number
  username: string
  is_admin: boolean
  role: string
}

export interface AuthWorkspace {
  id: string
  name: string
  slug: string
  icon?: string
  is_active?: boolean
}

interface AuthState {
  user: AuthUser | null
  loading: boolean
  workspaces: AuthWorkspace[]
  activeWorkspaceId: string | null
  login: (username: string, password: string) => Promise<void>
  logout: () => void
  switchWorkspace: (id: string) => Promise<void>
  refreshWorkspaces: () => Promise<void>
}

const AuthContext = createContext<AuthState | null>(null)

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error("useAuth must be used within <AuthProvider>")
  return ctx
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [workspaces, setWorkspaces] = useState<AuthWorkspace[]>([])
  const [activeWorkspaceId, setActiveId] = useState<string | null>(getActiveWorkspace())

  // Load the caller's workspaces and settle on an active one. The active id is
  // persisted to localStorage so the fetch interceptor can attach X-Workspace-Id
  // before any data query fires.
  const loadWorkspaces = useCallback(async () => {
    const res = await fetch("/api/workspaces")
    if (!res.ok) return
    const data = await res.json()
    const list: AuthWorkspace[] = data.workspaces ?? []
    setWorkspaces(list)

    const serverActive: string | null = data.active_id ?? null
    const chosen = serverActive ?? getActiveWorkspace() ?? list[0]?.id ?? null
    if (chosen) {
      setActiveWorkspace(chosen)
      setActiveId(chosen)
      // If the server has no active workspace recorded yet, persist our choice
      // (fire-and-forget; membership is already guaranteed for listed workspaces).
      if (!serverActive) {
        fetch("/api/workspaces/switch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ workspace_id: chosen }),
        }).catch(() => {})
      }
    }
  }, [])

  const loadUser = useCallback(async () => {
    if (!getToken()) {
      setUser(null)
      setLoading(false)
      return
    }
    try {
      const res = await fetch("/auth/me")
      if (!res.ok) {
        clearToken()
        setUser(null)
        return
      }
      setUser(await res.json())
      await loadWorkspaces()
    } catch {
      setUser(null)
    } finally {
      setLoading(false)
    }
  }, [loadWorkspaces])

  // Wire the global 401 handler and validate any persisted token on mount.
  useEffect(() => {
    onUnauthorized(() => {
      setUser(null)
      setWorkspaces([])
    })
    loadUser()
  }, [loadUser])

  const login = useCallback(async (username: string, password: string) => {
    const body = new URLSearchParams()
    body.set("username", username)
    body.set("password", password)
    const res = await fetch("/auth/token", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    })
    if (!res.ok) {
      const err = await res.json().catch(() => ({}))
      throw new Error(err.detail || "Incorrect username or password")
    }
    const data = await res.json()
    setToken(data.access_token)
    setLoading(true)
    await loadUser()
  }, [loadUser])

  const logout = useCallback(() => {
    clearToken()
    setActiveWorkspace(null)
    setUser(null)
    setWorkspaces([])
    setActiveId(null)
  }, [])

  const switchWorkspace = useCallback(async (id: string) => {
    const res = await fetch("/api/workspaces/switch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ workspace_id: id }),
    })
    if (!res.ok) {
      const err = await res.json().catch(() => ({}))
      throw new Error(err.detail || "Workspace access denied")
    }
    setActiveWorkspace(id)
    setActiveId(id)
    await loadWorkspaces()
  }, [loadWorkspaces])

  return (
    <AuthContext.Provider
      value={{
        user, loading, workspaces, activeWorkspaceId,
        login, logout, switchWorkspace, refreshWorkspaces: loadWorkspaces,
      }}
    >
      {children}
    </AuthContext.Provider>
  )
}
