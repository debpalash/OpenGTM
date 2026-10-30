import { useSyncExternalStore } from "react"

export type UIScalePreference = "auto" | "85" | "100"

declare global {
  interface Window {
    OpenGTMScale: {
      getSnapshot: () => string
      setPreference: (value: UIScalePreference) => void
      subscribe: (listener: () => void) => () => void
    }
  }
}

const subscribe = (listener: () => void) => window.OpenGTMScale.subscribe(listener)
const getSnapshot = () => window.OpenGTMScale.getSnapshot()
const getServerSnapshot = () => "auto:100"

export function useUIScale() {
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
  const [preference, percent] = snapshot.split(":")
  return {
    preference: preference as UIScalePreference,
    percent: Number(percent),
    setPreference: window.OpenGTMScale.setPreference
  }
}
