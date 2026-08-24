import { useState } from "react"
import { useLocation, useNavigate } from "react-router-dom"
import { ArrowRight, Check, KeyRound, Layers3, LockKeyhole, Search, Sparkles } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useAuth } from "@/lib/auth-context"

type AuthMode = "login" | "signup" | "reset"

const productSteps = [
  { icon: Search, label: "Find" },
  { icon: Sparkles, label: "Enrich" },
  { icon: Layers3, label: "Act" },
]

export default function LoginPage() {
  const { login } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const [mode, setMode] = useState<AuthMode>("login")
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [confirmPassword, setConfirmPassword] = useState("")
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const from = (location.state as { from?: string } | null)?.from ?? "/chat"

  const changeMode = (nextMode: AuthMode) => {
    setMode(nextMode)
    setError(null)
    setNotice(null)
    setPassword("")
    setConfirmPassword("")
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

  const title = mode === "login" ? "Welcome back" : mode === "signup" ? "Create your account" : "Recover access"
  const subtitle = mode === "login"
    ? "Sign in to continue."
    : mode === "signup"
      ? "Start with OpenGTM."
      : "Enter your username."

  return (
    <main className="min-h-screen bg-[#f7f5ef] text-[#171613] lg:grid lg:grid-cols-[minmax(0,1.08fr)_minmax(31rem,0.92fr)]">
      <section className="relative isolate min-h-[18rem] overflow-hidden bg-[#0d0c12] px-6 py-6 text-white sm:px-10 lg:flex lg:min-h-screen lg:flex-col lg:justify-between lg:px-14 lg:py-10 xl:px-20 xl:py-12">
        <div className="absolute inset-0 -z-20 bg-[radial-gradient(circle_at_15%_15%,rgba(98,104,242,.34),transparent_28%),radial-gradient(circle_at_86%_78%,rgba(32,207,175,.20),transparent_30%),radial-gradient(circle_at_64%_28%,rgba(125,132,255,.16),transparent_24%)]" />
        <div className="absolute -left-[18%] top-[28%] -z-10 h-[64%] w-[88%] rounded-[50%] border border-violet-300/20 bg-violet-500/10 blur-3xl" />
        <div className="absolute -bottom-[38%] -right-[18%] -z-10 h-[75%] w-[78%] rounded-full bg-[#20cfaf]/12 blur-3xl" />
        <div className="absolute inset-0 -z-10 opacity-[0.12] [background-image:linear-gradient(rgba(255,255,255,.18)_1px,transparent_1px),linear-gradient(90deg,rgba(255,255,255,.18)_1px,transparent_1px)] [background-size:48px_48px] [mask-image:linear-gradient(to_bottom,black,transparent_88%)]" />
        <svg className="pointer-events-none absolute inset-0 -z-10 h-full w-full opacity-50" viewBox="0 0 900 900" fill="none" aria-hidden="true">
          <path d="M-120 710C120 460 250 760 474 493C649 284 715 136 1005 238" stroke="url(#mesh-a)" strokeWidth="1.25" />
          <path d="M-80 786C174 530 337 850 551 563C687 380 797 278 1002 327" stroke="url(#mesh-b)" strokeWidth="1.25" />
          <path d="M34 902C237 674 419 893 637 641C744 517 844 445 998 446" stroke="url(#mesh-c)" strokeWidth="1.25" />
          <defs>
            <linearGradient id="mesh-a" x1="0" y1="0" x2="900" y2="0"><stop stopColor="#6268F2" stopOpacity="0"/><stop offset=".52" stopColor="#9A9FFF"/><stop offset="1" stopColor="#20CFAF" stopOpacity="0"/></linearGradient>
            <linearGradient id="mesh-b" x1="0" y1="0" x2="900" y2="0"><stop stopColor="#20CFAF" stopOpacity="0"/><stop offset=".58" stopColor="#6EE7D2"/><stop offset="1" stopColor="#6268F2" stopOpacity="0"/></linearGradient>
            <linearGradient id="mesh-c" x1="0" y1="0" x2="900" y2="0"><stop stopColor="#6268F2" stopOpacity="0"/><stop offset=".5" stopColor="#7B82FF"/><stop offset="1" stopColor="#20CFAF" stopOpacity="0"/></linearGradient>
          </defs>
        </svg>

        <div className="flex items-center gap-3">
          <img src="/opengtm-mark-v8.svg" alt="" aria-hidden="true" className="size-8 object-contain" />
          <span className="text-[1.08rem] font-semibold tracking-[-0.04em]">OpenGTM</span>
        </div>

        <div className="max-w-2xl py-10 lg:py-16">
          <div className="mb-6 inline-flex items-center gap-2 rounded-full border border-white/12 bg-white/[0.06] px-3 py-1.5 text-xs font-medium text-white/70 backdrop-blur-sm">
            <span className="size-1.5 rounded-full bg-[#20cfaf] shadow-[0_0_16px_#20cfaf]" />
            Open-source GTM
          </div>
          <h1 className="max-w-xl text-4xl font-medium leading-[1.02] tracking-[-0.055em] text-white sm:text-5xl lg:text-[4.25rem]">
            Build pipeline.
            <span className="block bg-gradient-to-r from-[#aeb2ff] via-[#d8daff] to-[#6ee7d2] bg-clip-text text-transparent">Not busywork.</span>
          </h1>
          <p className="mt-6 max-w-lg text-base leading-7 text-white/58 sm:text-lg">
            GTM agents for atomic teams.
          </p>

          <div className="mt-10 hidden max-w-xl grid-cols-3 gap-3 lg:grid">
            {productSteps.map(({ icon: Icon, label }, index) => (
              <div key={label} className="group rounded-2xl border border-white/10 bg-white/[0.055] p-4 backdrop-blur-md transition-colors hover:bg-white/[0.08]">
                <div className="mb-5 flex items-center justify-between">
                  <span className="flex size-8 items-center justify-center rounded-lg bg-white/10 text-white/80"><Icon className="size-4" /></span>
                  <span className="font-mono text-[10px] text-white/28">0{index + 1}</span>
                </div>
                <p className="text-sm font-medium text-white/90">{label}</p>
              </div>
            ))}
          </div>
        </div>

        <div className="hidden items-center gap-5 text-xs text-white/40 lg:flex">
          {["Local-first", "Composable", "Open source"].map((item) => (
            <span key={item} className="flex items-center gap-1.5"><Check className="size-3 text-[#8f82ff]" />{item}</span>
          ))}
        </div>
      </section>

      <section className="flex min-h-[calc(100vh-18rem)] items-center justify-center px-6 py-12 sm:px-12 lg:min-h-screen lg:px-16 xl:px-24">
        <div className="w-full max-w-[27rem]">
          <div className="mb-10">
            <p className="mb-3 text-[11px] font-semibold uppercase tracking-[0.18em] text-[#5b4cff]">
              {mode === "login" ? "Workspace access" : mode === "signup" ? "Get started" : "Account recovery"}
            </p>
            <h2 className="text-3xl font-semibold tracking-[-0.045em] text-[#171613] sm:text-[2.15rem]">{title}</h2>
            <p className="mt-3 text-sm leading-6 text-[#6e6a62]">{subtitle}</p>
          </div>

          <div className="mb-7 grid grid-cols-2 rounded-xl bg-[#ebe8df] p-1" role="tablist" aria-label="Authentication mode">
            <button type="button" role="tab" aria-selected={mode === "login"} onClick={() => changeMode("login")} className={`h-9 rounded-lg text-sm font-medium transition-all ${mode === "login" ? "bg-white text-[#171613] shadow-sm" : "text-[#777168] hover:text-[#171613]"}`}>
              Sign in
            </button>
            <button type="button" role="tab" aria-selected={mode === "signup"} onClick={() => changeMode("signup")} className={`h-9 rounded-lg text-sm font-medium transition-all ${mode === "signup" ? "bg-white text-[#171613] shadow-sm" : "text-[#777168] hover:text-[#171613]"}`}>
              Sign up
            </button>
          </div>

          <form onSubmit={handleSubmit} className="space-y-5">
            <div className="space-y-2">
              <Label htmlFor="username" className="text-xs font-medium text-[#45413b]">Username</Label>
              <Input id="username" autoFocus autoComplete="username" value={username} onChange={(event) => setUsername(event.target.value)} placeholder={mode === "reset" ? "Your OpenGTM username" : "Enter your username"} className="h-11 rounded-xl border-[#d8d4ca] !bg-white px-3.5 text-[#171613] shadow-[0_1px_0_rgba(0,0,0,.02)] placeholder:text-[#aaa49a] focus-visible:border-[#5b4cff] focus-visible:ring-[#5b4cff]/15" required />
            </div>

            {mode !== "reset" && (
              <div className="space-y-2">
                <div className="flex items-center justify-between">
                  <Label htmlFor="password" className="text-xs font-medium text-[#45413b]">Password</Label>
                  {mode === "login" && <button type="button" onClick={() => changeMode("reset")} className="text-xs font-medium text-[#5b4cff] hover:text-[#493bd9]">Forgot password?</button>}
                </div>
                <Input id="password" type="password" autoComplete={mode === "login" ? "current-password" : "new-password"} value={password} onChange={(event) => setPassword(event.target.value)} placeholder="••••••••" minLength={mode === "signup" ? 8 : undefined} className="h-11 rounded-xl border-[#d8d4ca] !bg-white px-3.5 text-[#171613] shadow-[0_1px_0_rgba(0,0,0,.02)] placeholder:text-[#aaa49a] focus-visible:border-[#5b4cff] focus-visible:ring-[#5b4cff]/15" required />
              </div>
            )}

            {mode === "signup" && (
              <div className="space-y-2">
                <Label htmlFor="confirm-password" className="text-xs font-medium text-[#45413b]">Confirm password</Label>
                <Input id="confirm-password" type="password" autoComplete="new-password" value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} placeholder="••••••••" minLength={8} className="h-11 rounded-xl border-[#d8d4ca] !bg-white px-3.5 text-[#171613] shadow-[0_1px_0_rgba(0,0,0,.02)] placeholder:text-[#aaa49a] focus-visible:border-[#5b4cff] focus-visible:ring-[#5b4cff]/15" required />
              </div>
            )}

            {error && <p className="rounded-xl bg-red-50 px-3.5 py-3 text-sm text-red-700" role="alert">{error}</p>}
            {notice && <p className="rounded-xl border border-[#dcd7ff] bg-[#f0eeff] px-3.5 py-3 text-sm leading-5 text-[#4438b8]" role="status">{notice}</p>}

            <Button type="submit" className="h-11 w-full rounded-xl bg-[#171613] text-sm text-white shadow-[0_8px_24px_rgba(23,22,19,.12)] hover:bg-[#5b4cff]" disabled={busy}>
              {busy ? "Signing in…" : mode === "login" ? "Enter OpenGTM" : mode === "signup" ? "Request account" : "Request password reset"}
              {!busy && <ArrowRight className="ml-1 size-4 transition-transform group-hover/button:translate-x-0.5" />}
            </Button>
          </form>

          {mode === "reset" && (
            <button type="button" onClick={() => changeMode("login")} className="mt-6 flex w-full items-center justify-center gap-2 text-sm font-medium text-[#5f5a52] hover:text-[#171613]">
              <KeyRound className="size-3.5" /> Back to sign in
            </button>
          )}

          <div className="mt-10 flex items-center justify-center gap-2 text-xs text-[#8c867c]">
            <LockKeyhole className="size-3.5" /> Credentials stay on your OpenGTM deployment
          </div>
        </div>
      </section>
    </main>
  )
}
