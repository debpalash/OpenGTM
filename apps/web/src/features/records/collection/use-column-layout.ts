import { useEffect, useState } from "react"
import { useAuth } from "@/lib/auth-context"
import { getActiveWorkspace } from "@/lib/auth"
import type { RecordKind } from "@/lib/records"
import {
  columnPreferenceKey,
  encodeColumnLayout,
  parseColumnLayout,
  type ColumnLayout
} from "./columns"

function readPreference(kind: RecordKind, key: string) {
  try {
    return {
      layout: parseColumnLayout(kind, localStorage.getItem(key)),
      temporary: false
    }
  } catch {
    return { layout: parseColumnLayout(kind, null), temporary: true }
  }
}

export function useColumnLayout(kind: RecordKind) {
  const { user } = useAuth()
  const key = columnPreferenceKey(
    user?.id ?? 0,
    getActiveWorkspace() ?? "",
    kind
  )
  const [state, setState] = useState(() => readPreference(kind, key))
  useEffect(() => {
    const update = (event: StorageEvent) => {
      if (event.key === key || event.key === null)
        setState(readPreference(kind, key))
    }
    window.addEventListener("storage", update)
    return () => window.removeEventListener("storage", update)
    // The authenticated workspace/record routes remount on scope changes.
  }, [key, kind])
  const apply = (layout: ColumnLayout) => {
    const encoded = encodeColumnLayout(kind, layout)
    const normalized = parseColumnLayout(kind, encoded)
    let temporary = false
    try {
      localStorage.setItem(key, encodeColumnLayout(kind, normalized))
    } catch {
      temporary = true
    }
    setState({ layout: normalized, temporary })
  }
  return { ...state, apply }
}
