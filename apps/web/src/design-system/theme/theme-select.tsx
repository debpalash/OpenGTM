import { useTheme, type ThemePreference } from "./use-theme"

export function ThemeSelect() {
  const { preference, setPreference } = useTheme()
  return <select
    aria-label="Color theme"
    value={preference}
    onChange={event => setPreference(event.target.value as ThemePreference)}
    className="h-10 rounded-md border border-border bg-background px-2 text-sm text-foreground outline-offset-2 hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring sm:h-7 sm:text-xs"
  >
    <option value="light">Light</option>
    <option value="dark">Dark</option>
    <option value="system">System</option>
  </select>
}
