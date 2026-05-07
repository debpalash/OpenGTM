import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { QueryClientProvider } from "@tanstack/react-query"
import { queryClient } from "./lib/query-client"
import App from "./App"
import "./index.css"

// ── Theme initialization (sync, before first paint) ──
// Reads stored preference; defaults to dark if nothing saved.
;(() => {
  const stored = localStorage.getItem("theme")
  if (stored === "light") {
    document.documentElement.classList.remove("dark")
  } else {
    document.documentElement.classList.add("dark")
    if (!stored) localStorage.setItem("theme", "dark")
  }
})()

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
)
