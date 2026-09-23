import { useEffect, useLayoutEffect, useState, type ReactNode } from "react"
import { useLocation, useNavigate } from "react-router-dom"
import { ArrowLeft, Building2, Eye, EyeOff, KeyRound, LockKeyhole, LogIn, User, UserPlus } from "lucide-react"

import { LoginScene } from "@/components/login-scene/login-scene"
import { useAuth } from "@/lib/auth-context"
import { cn } from "@/lib/utils"
import "./login.css"

type AuthMode = "login" | "signup" | "reset"

const COPY: Record<AuthMode, { icon: typeof LogIn; title: string; subtitle: string; submit: string }> = {
  login: {
    icon: LogIn,
    title: "Sign in to OpenGTM",
    subtitle: "Find, enrich and act on the accounts that matter. Your data stays on this deployment.",
    submit: "Sign in",
  },
  signup: {
    icon: UserPlus,
    title: "Request an account",
    subtitle: "Accounts on this self-hosted deployment are created by your workspace administrator.",
    submit: "Request account",
  },
  reset: {
    icon: KeyRound,
    title: "Recover access",
    subtitle: "Enter your username. Your OpenGTM administrator resets passwords on this deployment.",
    submit: "Request password reset",
  },
}

/** Inset field with a leading icon; the label stays available to assistive tech. */
function Field({ id, label, icon, children, trailing }: {
  id: string; label: string; icon: ReactNode; children: ReactNode; trailing?: ReactNode
}) {
  return (
    <div className="relative">
      <label htmlFor={id} className="sr-only">{label}</label>
      <span aria-hidden="true" className="pointer-events-none absolute top-1/2 left-3.5 -translate-y-1/2 text-[var(--t-font-color-tertiary)] [&_svg]:size-4">
        {icon}
      </span>
      {children}
      {trailing && <span className="absolute top-1/2 right-2 -translate-y-1/2">{trailing}</span>}
    </div>
  )
}

const GITHUB_REPO = "debpalash/OpenGTM"
/** OpenGTM on X; the button is hidden until VITE_OPENGTM_X_URL is set. */
const X_URL = (import.meta.env.VITE_OPENGTM_X_URL as string | undefined) || ""
const STARS_CACHE = "gtm-github-stars"

function GitHubMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true" className={className} fill="currentColor">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
    </svg>
  )
}

function XMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z" />
    </svg>
  )
}

function formatStars(count: number) {
  return count >= 1000 ? `${(count / 1000).toFixed(count >= 10000 ? 0 : 1)}k` : String(count)
}

/** Public star count, cached for an hour; the pill works without it. */
function useGitHubStars(repo: string) {
  const [stars, setStars] = useState<number | null>(() => {
    try {
      const cached = JSON.parse(localStorage.getItem(STARS_CACHE) || "null") as { count: number; at: number } | null
      return cached && Date.now() - cached.at < 3_600_000 ? cached.count : null
    } catch { return null }
  })
  useEffect(() => {
    if (stars !== null) return
    const controller = new AbortController()
    fetch(`https://api.github.com/repos/${repo}`, { signal: controller.signal, headers: { Accept: "application/vnd.github+json" } })
      .then(response => (response.ok ? response.json() : null))
      .then((data: { stargazers_count?: number } | null) => {
        if (typeof data?.stargazers_count !== "number") return
        setStars(data.stargazers_count)
        try { localStorage.setItem(STARS_CACHE, JSON.stringify({ count: data.stargazers_count, at: Date.now() })) } catch { /* private mode */ }
      })
      .catch(() => { /* offline or rate-limited: show the pill without a count */ })
    return () => controller.abort()
  }, [repo, stars])
  return stars
}

/** The sign-in page is designed for light only; restore the user's theme on leave. */
function useForceLightTheme() {
  useLayoutEffect(() => {
    const html = document.documentElement
    const theme = (window as unknown as { OpenGTMTheme?: {
      getSnapshot: () => string; setPreference: (value: string) => void; subscribe: (listener: () => void) => () => void
    } }).OpenGTMTheme
    const apply = () => {
      html.classList.remove("dark")
      html.classList.add("light")
      html.style.colorScheme = "light"
    }
    apply()
    const unsubscribe = theme?.subscribe(apply)
    return () => {
      unsubscribe?.()
      const preference = theme?.getSnapshot().split(":")[0]
      if (theme && preference) theme.setPreference(preference)
    }
  }, [])
}

const fieldClass =
  "gtm-login-field h-11 w-full rounded-xl border border-transparent bg-[#eef2f6] pr-3 pl-10 text-[15px] text-foreground " +
  "outline-none transition-[background-color,box-shadow] placeholder:text-[var(--t-font-color-tertiary)] " +
  "hover:bg-[#e8edf2] focus-visible:bg-background focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] "

export default function LoginPage() {
  useForceLightTheme()
  const stars = useGitHubStars(GITHUB_REPO)
  const { login } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const [mode, setMode] = useState<AuthMode>("login")
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [confirmPassword, setConfirmPassword] = useState("")
  const [showPassword, setShowPassword] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [workspaceSlug, setWorkspaceSlug] = useState("")

  const from = (location.state as { from?: string } | null)?.from ?? "/chat"
  const copy = COPY[mode]
  const ModeIcon = copy.icon

  const changeMode = (nextMode: AuthMode) => {
    setMode(nextMode)
    setError(null)
    setNotice(null)
    setPassword("")
    setConfirmPassword("")
    setShowPassword(false)
  }

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()
    setError(null)
    setNotice(null)

    if (mode === "signup") {
      if (password !== confirmPassword) {
        setError("Passwords do not match.")
        return
      }
      setNotice("Accounts are created by your OpenGTM workspace administrator on this self-hosted deployment.")
      return
    }

    if (mode === "reset") {
      setNotice("Password recovery is administrator-managed. Share this username with your OpenGTM administrator to reset access.")
      return
    }

    setBusy(true)
    try {
      await login(username.trim(), password)
      navigate(from, { replace: true })
    } catch (err) {
      setError(err instanceof Error ? err.message : "Login failed")
    } finally {
      setBusy(false)
    }
  }

  const passwordToggle = (
    <button type="button" onClick={() => setShowPassword(value => !value)}
      aria-label={showPassword ? "Hide password" : "Show password"} aria-pressed={showPassword}
      className="flex size-8 items-center justify-center rounded-lg text-[var(--t-font-color-tertiary)] outline-none transition-colors hover:bg-black/5 hover:text-foreground focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] [&_svg]:size-4">
      {showPassword ? <EyeOff /> : <Eye />}
    </button>
  )

  return (
    // `light` scopes every token on this page: Twenty's provider also marks its
    // wrapper with the app theme, and the nearest themed ancestor wins.
    <main className="light min-h-screen bg-[var(--t-background-secondary)] p-2 [color-scheme:light] sm:p-5">
      <div className="gtm-login-sky relative isolate flex min-h-[calc(100vh-1rem)] flex-col overflow-hidden rounded-[var(--gtm-radius-card)] border border-[var(--t-border-color-medium)] sm:min-h-[calc(100vh-2.5rem)] sm:rounded-[var(--gtm-radius-frame)]">
        <div className="gtm-login-clouds" aria-hidden="true" />
        <svg className="gtm-login-arcs pointer-events-none absolute top-1/2 left-1/2 -z-0 h-[1400px] w-[1400px] -translate-x-1/2 -translate-y-[30%]" viewBox="0 0 1400 1400" aria-hidden="true">
          <circle cx="700" cy="700" r="380" /><circle cx="700" cy="700" r="500" /><circle cx="700" cy="700" r="640" />
        </svg>
        <LoginScene />

        <header className="relative z-10 flex items-center justify-between gap-4 px-6 pt-6 sm:px-12 sm:pt-8">
          <div className="flex items-center gap-3">
            <span className="flex size-11 items-center justify-center rounded-[var(--t-border-radius-lg)] bg-[#1d1d1f] shadow-[0_1px_2px_rgb(0_0_0/0.2),0_6px_16px_rgb(0_0_0/0.12)]">
              <img src="/opengtm-mark-v8.svg" alt="" aria-hidden="true" className="size-7 object-contain" />
            </span>
            <span className="text-[22px] font-semibold tracking-[-0.025em] text-[#1d1d1f]">OpenGTM</span>
          </div>
          <nav aria-label="OpenGTM on the web" className="flex items-center gap-2">
            <a href={`https://github.com/${GITHUB_REPO}`} target="_blank" rel="noreferrer"
              aria-label={stars === null ? "Star OpenGTM on GitHub" : `Star OpenGTM on GitHub, ${stars} stars`}
              className="flex h-9 items-center gap-2 rounded-full bg-white/80 pr-1 pl-3 text-[13px] font-medium text-[#1d1d1f] shadow-[var(--gtm-control-shadow)] backdrop-blur-md transition-colors outline-none [corner-shape:round] hover:bg-white focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)]">
              <GitHubMark className="size-4" />
              <span>Star</span>
              <span className="rounded-full bg-[#1d1d1f]/[0.06] px-2 py-0.5 text-[12px] tabular-nums [corner-shape:round]">{stars === null ? "GitHub" : formatStars(stars)}</span>
            </a>
            {X_URL && (
              <a href={X_URL} target="_blank" rel="noreferrer" aria-label="OpenGTM on X"
                className="flex size-9 items-center justify-center rounded-full bg-white/80 text-[#1d1d1f] shadow-[var(--gtm-control-shadow)] backdrop-blur-md transition-colors outline-none [corner-shape:round] hover:bg-white focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)]">
                <XMark className="size-3.5" />
              </a>
            )}
          </nav>
        </header>

        <section className="pointer-events-none relative z-10 flex flex-1 items-center justify-center px-4 py-10">
          <div className="gtm-launch pointer-events-auto w-full max-w-[400px]">
            <div className="gtm-login-card relative rounded-[var(--gtm-radius-card)] border border-white/80 px-6 pt-8 pb-7 shadow-[var(--gtm-shadow-window)] backdrop-blur-xl sm:px-8">
              <div className="gtm-login-texture" aria-hidden="true" />
              <div className="relative">
                <div className="mx-auto mb-5 flex size-12 items-center justify-center rounded-[var(--gtm-radius-tile)] bg-white text-[#1d1d1f] shadow-[0_0_0_0.5px_rgb(0_0_0/0.06),0_4px_12px_rgb(0_0_0/0.08)]">
                  <ModeIcon className="size-5" aria-hidden="true" />
                </div>
                <h1 className="text-center text-[22px] font-semibold tracking-[-0.02em] text-foreground">{copy.title}</h1>
                <p className="mx-auto mt-2 max-w-[20rem] text-center text-sm leading-5 text-muted-foreground">{copy.subtitle}</p>

                <form onSubmit={handleSubmit} className="mt-6 grid gap-3">
                  <Field id="username" label="Username" icon={<User />}>
                    <input id="username" autoFocus autoComplete="username" required value={username}
                      onChange={event => setUsername(event.target.value)} placeholder="Username" className={fieldClass} />
                  </Field>

                  {mode !== "reset" && (
                    <Field id="password" label="Password" icon={<LockKeyhole />} trailing={passwordToggle}>
                      <input id="password" type={showPassword ? "text" : "password"} required
                        autoComplete={mode === "login" ? "current-password" : "new-password"}
                        minLength={mode === "signup" ? 8 : undefined} value={password}
                        onChange={event => setPassword(event.target.value)} placeholder="Password"
                        className={cn(fieldClass, "pr-11")} />
                    </Field>
                  )}

                  {mode === "signup" && (
                    <Field id="confirm-password" label="Confirm password" icon={<LockKeyhole />}>
                      <input id="confirm-password" type={showPassword ? "text" : "password"} required minLength={8}
                        autoComplete="new-password" value={confirmPassword}
                        onChange={event => setConfirmPassword(event.target.value)} placeholder="Confirm password"
                        className={fieldClass} />
                    </Field>
                  )}

                  {mode === "login" && (
                    <div className="-mt-1 flex justify-end">
                      <button type="button" onClick={() => changeMode("reset")}
                        className="rounded text-[13px] font-medium text-foreground/80 outline-none hover:text-foreground focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)]">
                        Forgot password?
                      </button>
                    </div>
                  )}

                  {error && <p role="alert" className="rounded-xl bg-destructive/10 px-3.5 py-2.5 text-[13px] text-destructive">{error}</p>}
                  {notice && <p role="status" className="rounded-xl bg-[var(--gtm-accent)]/10 px-3.5 py-2.5 text-[13px] leading-5 text-foreground">{notice}</p>}

                  <button type="submit" disabled={busy}
                    className="mt-1 h-11 w-full rounded-xl bg-[linear-gradient(to_bottom,#3a3a3e,#161618)] text-[15px] font-medium text-white shadow-[inset_0_1px_0_rgb(255_255_255/0.14),0_1px_2px_rgb(0_0_0/0.25),0_6px_16px_rgb(0_0_0/0.14)] outline-none transition-[filter,box-shadow] hover:brightness-125 focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] active:brightness-95 disabled:opacity-60">
                    {busy ? "Signing in…" : copy.submit}
                  </button>
                </form>

                {mode === "login" && (
                  <>
                    <div className="mt-6 flex items-center gap-3" role="presentation">
                      <span className="gtm-login-divider flex-1" />
                      <span className="text-xs text-muted-foreground">Or continue with SSO</span>
                      <span className="gtm-login-divider flex-1" />
                    </div>
                    <form className="mt-4 flex flex-col gap-2 sm:flex-row" onSubmit={event => {
                      event.preventDefault()
                      if (workspaceSlug.trim()) window.location.assign(`/auth/sso/${encodeURIComponent(workspaceSlug.trim())}/login`)
                    }}>
                      <div className="flex-1">
                        <Field id="sso-workspace" label="SSO workspace slug" icon={<Building2 />}>
                          <input id="sso-workspace" value={workspaceSlug} onChange={event => setWorkspaceSlug(event.target.value)}
                            placeholder="workspace-slug" autoComplete="organization" className={cn(fieldClass, "h-10")} />
                        </Field>
                      </div>
                      <button type="submit" disabled={!workspaceSlug.trim()}
                        className="h-10 shrink-0 rounded-xl bg-[var(--gtm-control-bezel)] px-3.5 text-[13px] font-medium text-foreground shadow-[var(--gtm-control-shadow)] outline-none transition-[filter] hover:brightness-[0.97] focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] disabled:opacity-50">
                        Continue with SSO
                      </button>
                    </form>
                  </>
                )}

                <p className="mt-6 text-center text-[13px] text-muted-foreground">
                  {mode === "login" && <>New to OpenGTM? <button type="button" onClick={() => changeMode("signup")} className="font-medium text-foreground hover:underline">Request an account</button></>}
                  {mode === "signup" && <>Already have an account? <button type="button" onClick={() => changeMode("login")} className="font-medium text-foreground hover:underline">Sign in</button></>}
                  {mode === "reset" && <button type="button" onClick={() => changeMode("login")} className="inline-flex items-center gap-1.5 font-medium text-foreground hover:underline"><ArrowLeft className="size-3.5" aria-hidden="true" />Back to sign in</button>}
                </p>
              </div>
            </div>
            <p className="mt-5 flex items-center justify-center gap-1.5 text-xs text-[#1d1d1f]/60">
              <LockKeyhole className="size-3.5" aria-hidden="true" /> Credentials stay on your OpenGTM deployment
            </p>
          </div>
        </section>

        <footer className="relative z-10 px-6 pb-5 text-center text-[11px] text-[#1d1d1f]/55 sm:px-12">
          Inspired by{" "}
          <a href="https://dribbble.com/BagasPrayogo" target="_blank" rel="noreferrer" className="underline decoration-current/30 underline-offset-2 hover:text-[#1d1d1f]">Bagas Prayogo</a>
          {" "}(sign-in design) and{" "}
          <a href="https://dribbble.com/koniu" target="_blank" rel="noreferrer" className="underline decoration-current/30 underline-offset-2 hover:text-[#1d1d1f]">koniu</a>
          {" "}(mascot).
        </footer>
      </div>
    </main>
  )
}
