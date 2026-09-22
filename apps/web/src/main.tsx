import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { QueryClientProvider } from "@tanstack/react-query"
import { queryClient } from "./lib/query-client"
import { installFetchInterceptor } from "./lib/auth"
import App from "./App"
import { ThemeProvider } from "./design-system/theme/theme-provider"
import "./index.css"

// Attach Authorization + X-Workspace-Id to every API request, and funnel 401s
// through the auth context's logout handler. Must run before any fetch fires.
installFetchInterceptor()

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider><App /></ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
