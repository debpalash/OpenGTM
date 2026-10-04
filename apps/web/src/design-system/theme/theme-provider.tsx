import type { ReactNode } from "react"
import { useTheme } from "./use-theme"

export function ThemeProvider({ children }: { children: ReactNode }) {
  const { colorScheme } = useTheme()
  // The boot store (public/theme.js) alone owns the root class. This
  // layout-neutral wrapper also carries the active scheme class, so tokens and
  // `dark:` variants resolve from the nearest themed ancestor as before.
  return <div className={colorScheme} style={{ display: "contents" }}>
    {children}
  </div>
}
