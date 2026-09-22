import { useTheme, type ThemePreference } from "./use-theme"

export function ThemeSelect() {
  const { preference, setPreference } = useTheme()
  return <select
    aria-label="Color theme"
    value={preference}
    onChange={event => setPreference(event.target.value as ThemePreference)}
    className="h-11 rounded-md border border-input bg-background px-2 text-base text-foreground outline-offset-2 focus-visible:outline-2 focus-visible:outline-ring sm:h-9 sm:text-xs"
  >
    <option value="light">Light</option>
    <option value="dark">Dark</option>
    <option value="system">System</option>
  </select>
}
