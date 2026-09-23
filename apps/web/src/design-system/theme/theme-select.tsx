import { NativeSelect } from "@/components/ui/native-select"
import { useTheme, type ThemePreference } from "./use-theme"

export function ThemeSelect() {
  const { preference, setPreference } = useTheme()
  return <NativeSelect
    aria-label="Color theme"
    value={preference}
    onChange={event => setPreference(event.target.value as ThemePreference)}
    className="w-24"
  >
    <option value="light">Light</option>
    <option value="dark">Dark</option>
    <option value="system">System</option>
  </NativeSelect>
}
