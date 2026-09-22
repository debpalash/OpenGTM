import { describe, expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { runInNewContext } from "node:vm"

const script = readFileSync(new URL("../public/theme.js", import.meta.url), "utf8")

function boot(saved: string | null = null, systemDark = false, blocked = false) {
  const classes = new Set<string>()
  const events: Record<string, (event?: { key: string | null }) => void> = {}
  const media = { matches: systemDark, addEventListener: (_: string, listener: () => void) => { events.media = listener } }
  const storage = new Map(saved ? [["theme", saved]] : [])
  const root = { classList: { toggle: (name: string, enabled: boolean) => enabled ? classes.add(name) : classes.delete(name) }, style: { colorScheme: "" } }
  const window = {
    matchMedia: () => media,
    localStorage: {
      getItem: (key: string) => { if (blocked) throw Error("Blocked"); return storage.get(key) ?? null },
      setItem: (key: string, value: string) => { if (blocked) throw Error("Blocked"); storage.set(key, value) },
    },
    addEventListener: (event: string, listener: () => void) => { events[event] = listener },
    OpenGTMTheme: undefined as undefined | { getSnapshot: () => string; setPreference: (value: string) => void; subscribe: (listener: () => void) => () => void },
  }
  runInNewContext(script, { window, document: { documentElement: root }, Set })
  return { store: window.OpenGTMTheme!, media, events, storage, root, classes }
}

describe("shared theme boot store", () => {
  test.each([null, "invalid", "dark"])("preserves default dark for %s", saved => {
    const { store, classes } = boot(saved)
    expect(store.getSnapshot()).toBe("dark:dark")
    expect([...classes]).toEqual(["dark"])
  })
  test("honors legacy light preference before React mounts", () => {
    const { store, root, classes } = boot("light", true)
    expect(store.getSnapshot()).toBe("light:light")
    expect(root.style.colorScheme).toBe("light")
    expect([...classes]).toEqual(["light"])
  })
  test("follows system changes only in system mode", () => {
    const { store, media, events } = boot("system", false)
    expect(store.getSnapshot()).toBe("system:light")
    media.matches = true
    events.media()
    expect(store.getSnapshot()).toBe("system:dark")
    store.setPreference("light")
    events.media()
    expect(store.getSnapshot()).toBe("light:light")
  })
  test("persists, notifies, and unsubscribes", () => {
    const { store, storage } = boot()
    let calls = 0
    const unsubscribe = store.subscribe(() => calls++)
    store.setPreference("light")
    expect(storage.get("theme")).toBe("light")
    expect(calls).toBe(1)
    unsubscribe()
    store.setPreference("dark")
    expect(calls).toBe(1)
  })
  test("updates across tabs and handles cleared preferences", () => {
    const { store, storage, events } = boot()
    storage.set("theme", "light")
    events.storage({ key: "theme" })
    expect(store.getSnapshot()).toBe("light:light")
    storage.clear()
    events.storage({ key: null })
    expect(store.getSnapshot()).toBe("dark:dark")
  })
  test("storage denial does not prevent theme selection", () => {
    const { store } = boot(null, false, true)
    store.setPreference("light")
    expect(store.getSnapshot()).toBe("light:light")
  })
})
