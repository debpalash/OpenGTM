// The checklist step model: pure functions from the server state to what the card renders, plus the
// dismissal flag. No React here so it is unit-testable.
import type { OnboardingState, StepId } from "./onboarding-client"

export type StepAction =
  | { kind: "link"; label: string; to: string }
  | { kind: "sample"; label: string }

export interface ChecklistStep {
  id: StepId
  title: string
  why: string
  done: boolean
  actions: StepAction[]
}

const COPY: Record<StepId, { title: string; why: string }> = {
  llm_provider: {
    title: "Connect an AI provider",
    why: "The assistant and AI columns need a language model; a free OpenRouter or Google AI key is enough."
  },
  data_source: {
    title: "Turn on a data source",
    why: "Sources are where new companies and people come from when you search or collect."
  },
  data: {
    title: "Add your first records",
    why: "Enrichment needs something to work on: import your own list or try the sample."
  },
  enrichment: {
    title: "Run your first enrichment",
    why: "This fills in emails, phones and company details, which is the time saving."
  }
}

/** Steps in checklist order with their copy and the one place to do each. Done steps carry no actions. */
export function buildSteps(state: OnboardingState): ChecklistStep[] {
  const sample = state.sample_workbook_id
  const actions = (id: StepId): StepAction[] => {
    switch (id) {
      case "llm_provider":
        return [{ kind: "link", label: "Open AI provider settings", to: "/settings" }]
      case "data_source":
        return [{ kind: "link", label: "Open data sources", to: "/sources" }]
      case "data":
        return [
          { kind: "link", label: "Import a CSV", to: "/companies?import=1" },
          sample
            ? { kind: "link", label: "Open sample workbook", to: `/workbooks/${encodeURIComponent(sample)}` }
            : { kind: "sample", label: "Load sample workbook" }
        ]
      case "enrichment":
        return [
          sample
            ? { kind: "link", label: "Run the sample workbook", to: `/workbooks/${encodeURIComponent(sample)}` }
            : { kind: "link", label: "Open workbooks", to: "/workbooks" }
        ]
    }
  }
  return state.steps.map((step) => ({
    id: step.id,
    ...COPY[step.id],
    done: step.done,
    actions: step.done ? [] : actions(step.id)
  }))
}

/** "2 of 4 done": text, so progress never relies on colour. */
export function progressLabel(state: Pick<OnboardingState, "done_count" | "total">): string {
  return `${state.done_count} of ${state.total} done`
}

/** Polite announcement for a change between two states, or "" when nothing changed. */
export function describeChange(prev: OnboardingState, next: OnboardingState): string {
  const before = new Set(prev.steps.filter((s) => s.done).map((s) => s.id))
  const finished = next.steps.filter((s) => s.done && !before.has(s.id)).map((s) => COPY[s.id].title)
  if (finished.length === 0 && prev.done_count === next.done_count) return ""
  if (next.complete) return "Setup complete. All steps are done."
  const lead = finished.length ? `${finished.join(", ")}: done. ` : ""
  return `${lead}${progressLabel(next)}.`
}

export function showChecklist(state: OnboardingState | undefined, dismissed: boolean): boolean {
  return !!state && !state.complete && !dismissed
}

// Dismissal lives in localStorage (per browser). Every access is guarded: storage can throw or be absent
// (private windows, blocked site data), and the checklist must then simply stay visible.
export const DISMISS_KEY = "opengtm.onboarding.dismissed"
export const DISMISS_EVENT = "opengtm:onboarding-dismissal"

// In-memory copy so a dismissal still holds for this page session when storage is unavailable.
let memoryDismissed = false

export function readDismissed(storage?: Pick<Storage, "getItem">): boolean {
  if (memoryDismissed) return true
  try {
    const s = storage ?? (typeof localStorage === "undefined" ? undefined : localStorage)
    return s?.getItem(DISMISS_KEY) === "1"
  } catch {
    return false
  }
}

export function writeDismissed(value: boolean, storage?: Pick<Storage, "setItem" | "removeItem">): void {
  memoryDismissed = value
  try {
    const s = storage ?? (typeof localStorage === "undefined" ? undefined : localStorage)
    if (value) s?.setItem(DISMISS_KEY, "1")
    else s?.removeItem(DISMISS_KEY)
  } catch {
    // Storage unavailable: the in-memory notification below still updates the page.
  }
  try {
    if (typeof window !== "undefined") window.dispatchEvent(new Event(DISMISS_EVENT))
  } catch {
    // no window (tests)
  }
}
