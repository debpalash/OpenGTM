import { useSyncExternalStore } from "react"

export type ThemePreference = "light" | "dark" | "system"
export type ColorScheme = "light" | "dark"

declare global {
  interface Window {
    OpenGTMTheme: {
      getSnapshot: () => string
      setPreference: (value: ThemePreference) => void
      subscribe: (listener: () => void) => () => void
    }
  }
}

const subscribe = (listener: () => void) => window.OpenGTMTheme.subscribe(listener)
const getSnapshot = () => window.OpenGTMTheme.getSnapshot()
const getServerSnapshot = () => "dark:dark"

export function useTheme() {
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
  const [preference, colorScheme] = snapshot.split(":") as [ThemePreference, ColorScheme]
  return { preference, colorScheme, setPreference: window.OpenGTMTheme.setPreference }
}
