import type { ReactNode } from "react"
import { ThemeProvider as TwentyThemeProvider } from "twenty-ui/theme-constants"
import { useTheme } from "./use-theme"

export function ThemeProvider({ children }: { children: ReactNode }) {
  const { colorScheme } = useTheme()
  // The boot store alone owns the root class. Twenty scopes its portals and
  // component context to the same scheme without a second root controller.
  return <TwentyThemeProvider colorScheme={colorScheme} applyToRoot={false}>
    {children}
  </TwentyThemeProvider>
}
