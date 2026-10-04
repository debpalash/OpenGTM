import { RouterProvider } from "@tanstack/react-router"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { AuthProvider, useAuth } from "@/lib/auth-context"
import { router } from "@/router"

function RoutedApp() {
  const { user, loading, activeWorkspaceId } = useAuth()
  return <RouterProvider router={router} context={{ authReady: !loading && !!user && !!activeWorkspaceId }} />
}

export default function App() {
  return (
    <TooltipProvider>
      <AuthProvider>
        <RoutedApp />
        <Toaster />
      </AuthProvider>
    </TooltipProvider>
  )
}
