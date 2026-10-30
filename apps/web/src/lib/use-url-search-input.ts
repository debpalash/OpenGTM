import { useEffect, useRef, useState } from "react"

/** Keep typing responsive while router transitions write the shareable URL.
 * Back/Forward adopts the URL after the field's own writes have settled. */
export function useUrlSearchInput(urlValue: string, write: (value: string) => void) {
  const [value, setValue] = useState(urlValue)
  const pending = useRef<{ value: string; at: number }[]>([])
  useEffect(() => {
    const recent = pending.current.filter(entry => Date.now() - entry.at < 1500)
    const echoed = recent.findIndex(entry => entry.value === urlValue)
    pending.current = echoed >= 0 ? recent.slice(echoed + 1) : recent
    if (echoed < 0 && recent.length === 0) setValue(urlValue)
  }, [urlValue])
  return {
    value,
    onChange(next: string) {
      setValue(next)
      if (next !== urlValue) pending.current.push({ value: next, at: Date.now() })
      write(next)
    }
  }
}
