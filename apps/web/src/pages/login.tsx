import { useState, type ReactNode } from "react"
import { useLocation, useNavigate } from "react-router-dom"
import { ArrowLeft, Building2, Eye, EyeOff, KeyRound, LockKeyhole, LogIn, User, UserPlus } from "lucide-react"

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

const fieldClass =
  "h-11 w-full rounded-xl border border-transparent bg-[#eef2f6] pr-3 pl-10 text-[15px] text-foreground " +
  "outline-none transition-[background-color,box-shadow] placeholder:text-[var(--t-font-color-tertiary)] " +
  "hover:bg-[#e8edf2] focus-visible:bg-background focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] " +
  "dark:bg-white/[0.07] dark:hover:bg-white/[0.1] dark:focus-visible:bg-white/[0.1]"

export default function LoginPage() {
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
      className="flex size-8 items-center justify-center rounded-lg text-[var(--t-font-color-tertiary)] outline-none transition-colors hover:bg-black/5 hover:text-foreground focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] dark:hover:bg-white/10 [&_svg]:size-4">
      {showPassword ? <EyeOff /> : <Eye />}
    </button>
  )

  return (
    <main className="min-h-screen bg-[var(--t-background-secondary)] p-2 sm:p-5">
      <div className="gtm-login-sky relative isolate flex min-h-[calc(100vh-1rem)] flex-col overflow-hidden rounded-[var(--gtm-radius-card)] border border-[var(--t-border-color-medium)] sm:min-h-[calc(100vh-2.5rem)] sm:rounded-[var(--gtm-radius-frame)]">
        <div className="gtm-login-clouds" aria-hidden="true" />
        <svg className="gtm-login-arcs pointer-events-none absolute top-1/2 left-1/2 -z-0 h-[1400px] w-[1400px] -translate-x-1/2 -translate-y-[30%]" viewBox="0 0 1400 1400" aria-hidden="true">
          <circle cx="700" cy="700" r="380" /><circle cx="700" cy="700" r="500" /><circle cx="700" cy="700" r="640" />
        </svg>

        <header className="relative z-10 flex items-center gap-2.5 px-6 pt-6 sm:px-12 sm:pt-8">
          <span className="flex size-8 items-center justify-center rounded-[var(--t-border-radius-md)] bg-[#1d1d1f] shadow-[0_1px_2px_rgb(0_0_0/0.2)] dark:bg-white">
            <img src="/opengtm-mark-v8.svg" alt="" aria-hidden="true" className="size-5 object-contain" />
          </span>
          <span className="text-[17px] font-semibold tracking-[-0.02em] text-[#1d1d1f] dark:text-white">OpenGTM</span>
        </header>

        <section className="relative z-10 flex flex-1 items-center justify-center px-4 py-10">
          <div className="gtm-launch w-full max-w-[400px]">
            <div className="gtm-login-card relative rounded-[var(--gtm-radius-card)] border border-white/80 px-6 pt-8 pb-7 shadow-[var(--gtm-shadow-window)] backdrop-blur-xl sm:px-8 dark:border-white/10">
              <div className="gtm-login-texture" aria-hidden="true" />
              <div className="relative">
                <div className="mx-auto mb-5 flex size-12 items-center justify-center rounded-[var(--gtm-radius-tile)] bg-white text-[#1d1d1f] shadow-[0_0_0_0.5px_rgb(0_0_0/0.06),0_4px_12px_rgb(0_0_0/0.08)] dark:bg-white/10 dark:text-white dark:shadow-none">
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
                    className="mt-1 h-11 w-full rounded-xl bg-[linear-gradient(to_bottom,#3a3a3e,#161618)] text-[15px] font-medium text-white shadow-[inset_0_1px_0_rgb(255_255_255/0.14),0_1px_2px_rgb(0_0_0/0.25),0_6px_16px_rgb(0_0_0/0.14)] outline-none transition-[filter,box-shadow] hover:brightness-125 focus-visible:shadow-[0_0_0_3px_var(--gtm-focus-ring)] active:brightness-95 disabled:opacity-60 dark:bg-[linear-gradient(to_bottom,#ffffff,#e6e6ea)] dark:text-[#1d1d1f] dark:hover:brightness-95">
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
            <p className="mt-5 flex items-center justify-center gap-1.5 text-xs text-[#1d1d1f]/60 dark:text-white/50">
              <LockKeyhole className="size-3.5" aria-hidden="true" /> Credentials stay on your OpenGTM deployment
            </p>
          </div>
        </section>
      </div>
    </main>
  )
}
