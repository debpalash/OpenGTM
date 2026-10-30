import {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  useRef,
  type ReactNode
} from "react"
import { useLeaveConfirmation } from "@/features/navigation/leave-guard-provider"
import { queryClient } from "./query-client"
import {
  getToken,
  setToken,
  clearToken,
  getActiveWorkspace,
  setActiveWorkspace,
  onUnauthorized
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
  /** First-run setup: null until GET /auth/setup/status answers. */
  setupRequired: boolean | null
  /** Whether the setup form must carry the SETUP_TOKEN printed in the API log. */
  setupTokenRequired: boolean
  login: (username: string, password: string) => Promise<void>
  completeSetup: (input: SetupInput) => Promise<void>
  logout: () => Promise<void>
  switchWorkspace: (id: string) => Promise<boolean>
  refreshWorkspaces: () => Promise<void>
}

export interface SetupInput {
  username: string
  password: string
  workspaceName?: string
  setupToken?: string
}

const AuthContext = createContext<AuthState | null>(null)

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error("useAuth must be used within <AuthProvider>")
  return ctx
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const { request: confirmLeave, cancel: cancelLeave } = useLeaveConfirmation()
  const changingContext = useRef(false)
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [workspaces, setWorkspaces] = useState<AuthWorkspace[]>([])
  const [activeWorkspaceId, setActiveId] = useState<string | null>(
    getActiveWorkspace()
  )
  const [setupRequired, setSetupRequired] = useState<boolean | null>(null)
  const [setupTokenRequired, setSetupTokenRequired] = useState(false)

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
    const preferred = getActiveWorkspace()
    const chosen = list.some((workspace) => workspace.id === preferred)
      ? preferred
      : list.some((workspace) => workspace.id === serverActive)
        ? serverActive
        : (list[0]?.id ?? null)
    if (chosen) {
      setActiveWorkspace(chosen)
      setActiveId(chosen)
      // If the server has no active workspace recorded yet, persist our choice
      // (fire-and-forget; membership is already guaranteed for listed workspaces).
      if (!serverActive) {
        fetch("/api/workspaces/switch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ workspace_id: chosen })
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
    const fragment = new URLSearchParams(window.location.hash.slice(1))
    const ssoToken = fragment.get("sso_access_token")
    if (ssoToken) {
      setToken(ssoToken)
      window.history.replaceState(
        {},
        "",
        window.location.pathname + window.location.search
      )
    }
    onUnauthorized(() => {
      cancelLeave()
      // Revoked sessions cannot wait for a discard decision. Clear private data
      // immediately; explicit user sign-out uses the leave guard below.
      queryClient.clear()
      setUser(null)
      setWorkspaces([])
      setActiveId(null)
    })
    loadUser()
    // Unreachable status endpoint: fall back to the normal login flow.
    fetch("/auth/setup/status")
      .then((res) => (res.ok ? res.json() : null))
      .then(
        (
          data: { setup_required?: boolean; token_required?: boolean } | null
        ) => {
          setSetupRequired(data?.setup_required === true)
          setSetupTokenRequired(data?.token_required === true)
        }
      )
      .catch(() => setSetupRequired(false))
  }, [loadUser, cancelLeave])

  const login = useCallback(
    async (username: string, password: string) => {
      const body = new URLSearchParams()
      body.set("username", username)
      body.set("password", password)
      const res = await fetch("/auth/token", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        throw new Error(err.detail || "Incorrect username or password")
      }
      const data = await res.json()
      setToken(data.access_token)
      setLoading(true)
      await loadUser()
    },
    [loadUser]
  )

  const completeSetup = useCallback(
    async (input: SetupInput) => {
      const res = await fetch("/auth/setup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username: input.username,
          password: input.password,
          workspace_name: input.workspaceName || undefined,
          setup_token: input.setupToken || undefined
        })
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        if (res.status === 409) setSetupRequired(false)
        const detail = Array.isArray(err.detail)
          ? err.detail
              .map((d: { msg?: string }) => d.msg)
              .filter(Boolean)
              .join(" ")
          : err.detail
        throw new Error(detail || "Setup failed")
      }
      const data = await res.json()
      setToken(data.access_token)
      setSetupRequired(false)
      setLoading(true)
      await loadUser()
    },
    [loadUser]
  )

  // Session identity is shared across tabs; workspace selection is tab-bound.
  useEffect(() => {
    const changed = (event: StorageEvent) => {
      if (
        event.storageArea !== localStorage ||
        (event.key !== "yupcha_token" && event.key !== null)
      )
        return
      cancelLeave()
      void queryClient.cancelQueries()
      queryClient.clear()
      setUser(null)
      setWorkspaces([])
      setActiveId(null)
      setLoading(true)
      void loadUser()
    }
    window.addEventListener("storage", changed)
    return () => window.removeEventListener("storage", changed)
  }, [loadUser, cancelLeave])

  const logout = useCallback(async () => {
    if (changingContext.current) return
    changingContext.current = true
    try {
      if (!(await confirmLeave("Sign out"))) return
      await queryClient.cancelQueries()
      queryClient.clear()
      clearToken()
      setActiveWorkspace(null)
      setUser(null)
      setWorkspaces([])
      setActiveId(null)
    } finally {
      changingContext.current = false
    }
  }, [confirmLeave])

  const switchWorkspace = useCallback(
    async (id: string) => {
      if (id === getActiveWorkspace()) return true
      if (changingContext.current) return false
      changingContext.current = true
      try {
        if (!(await confirmLeave("Switch workspaces"))) return false
        const res = await fetch("/api/workspaces/switch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ workspace_id: id })
        })
        if (!res.ok) {
          const err = await res.json().catch(() => ({}))
          throw new Error(err.detail || "Workspace access denied")
        }
        // Query keys are not yet workspace-scoped. Never reuse the previous
        // workspace's records, selection sources, or in-flight query results.
        await queryClient.cancelQueries()
        queryClient.clear()
        setActiveWorkspace(id)
        setActiveId(id)
        await loadWorkspaces()
        return true
      } finally {
        changingContext.current = false
      }
    },
    [loadWorkspaces, confirmLeave]
  )

  return (
    <AuthContext.Provider
      value={{
        user,
        loading: loading || setupRequired === null,
        workspaces,
        activeWorkspaceId,
        setupRequired,
        setupTokenRequired,
        login,
        completeSetup,
        logout,
        switchWorkspace,
        refreshWorkspaces: loadWorkspaces
      }}
    >
      {children}
    </AuthContext.Provider>
  )
}
