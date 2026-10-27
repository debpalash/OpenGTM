import { Navigate } from "react-router-dom"
import { LoaderCircle } from "lucide-react"

import { useAuth } from "@/lib/auth-context"
import LoginPage from "@/pages/login"

/** First visit to an empty instance: create the admin account. Shares the sign-in layout. */
export default function SetupPage() {
  const { user, loading, setupRequired } = useAuth()
  if (loading) {
    return (
      <div role="status" className="flex min-h-screen items-center justify-center text-muted-foreground">
        <LoaderCircle className="size-5 animate-spin" aria-label="Loading" />
      </div>
    )
  }
  if (user) return <Navigate to="/chat" replace />
  if (!setupRequired) return <Navigate to="/login" replace />
  return <LoginPage setup />
}
