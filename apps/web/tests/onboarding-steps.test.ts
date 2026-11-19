import { beforeEach, expect, test } from "bun:test"
import type { OnboardingState, StepId } from "../src/features/onboarding/onboarding-client"
import {
  DISMISS_KEY,
  buildSteps,
  describeChange,
  progressLabel,
  readDismissed,
  sampleLoadedMessage,
  showChecklist,
  writeDismissed
} from "../src/features/onboarding/onboarding-steps"

const make = (done: StepId[], sample: string | null = null): OnboardingState => {
  const ids: StepId[] = ["llm_provider", "data_source", "data", "enrichment"]
  return {
    steps: ids.map((id) => ({ id, done: done.includes(id) })),
    done_count: done.length,
    total: 4,
    complete: done.length === 4,
    sample_workbook_id: sample
  }
}

test("progress is text, not colour", () => {
  expect(progressLabel(make(["llm_provider", "data_source"]))).toBe("2 of 4 done")
  expect(progressLabel(make([]))).toBe("0 of 4 done")
})

test("steps keep checklist order and each has a reason", () => {
  const steps = buildSteps(make([]))
  expect(steps.map((s) => s.id)).toEqual(["llm_provider", "data_source", "data", "enrichment"])
  for (const step of steps) {
    expect(step.title.length).toBeGreaterThan(0)
    expect(step.why.endsWith(".")).toBe(true)
  }
})

test("each open step has one place to go; done steps have none", () => {
  const steps = buildSteps(make(["llm_provider"]))
  expect(steps[0].actions).toEqual([])
  expect(steps[1].actions).toEqual([{ kind: "link", label: "Open data sources", to: "/sources" }])
  expect(buildSteps(make([]))[0].actions).toEqual([
    { kind: "link", label: "Open AI provider settings", to: "/settings" }
  ])
})

test("the data step offers an import and the sample, then the sample workbook once it exists", () => {
  const before = buildSteps(make([]))[2].actions
  expect(before).toEqual([
    { kind: "link", label: "Import a CSV", to: "/companies?import=1" },
    { kind: "sample", label: "Load sample workbook" }
  ])
  const after = buildSteps(make([], "sample-abc"))
  expect(after[2].actions[1]).toEqual({ kind: "link", label: "Open sample workbook", to: "/workbooks/sample-abc" })
  expect(after[3].actions).toEqual([{ kind: "link", label: "Run the sample workbook", to: "/workbooks/sample-abc" }])
  expect(buildSteps(make([]))[3].actions).toEqual([{ kind: "link", label: "Open workbooks", to: "/workbooks" }])
})

test("changes are described for the live region, and unchanged state is silent", () => {
  const a = make(["llm_provider"])
  expect(describeChange(a, a)).toBe("")
  expect(describeChange(a, make(["llm_provider", "data_source"]))).toBe("Turn on a data source: done. 2 of 4 done.")
  expect(describeChange(make(["llm_provider", "data_source", "data"]), make(["llm_provider", "data_source", "data", "enrichment"]))).toBe(
    "Setup complete. All steps are done."
  )
  // a step undone is also reported
  expect(describeChange(make(["llm_provider", "data_source"]), a)).toBe("1 of 4 done.")
})

test("the checklist shows until complete or dismissed", () => {
  expect(showChecklist(undefined, false)).toBe(false)
  expect(showChecklist(make([]), false)).toBe(true)
  expect(showChecklist(make([]), true)).toBe(false)
  expect(showChecklist(make(["llm_provider", "data_source", "data", "enrichment"]), false)).toBe(false)
})

class MemoryStorage {
  items = new Map<string, string>()
  getItem(k: string) {
    return this.items.get(k) ?? null
  }
  setItem(k: string, v: string) {
    this.items.set(k, v)
  }
  removeItem(k: string) {
    this.items.delete(k)
  }
}

beforeEach(() => writeDismissed(false, new MemoryStorage()))

test("dismissal round-trips through storage and can be undone", () => {
  const storage = new MemoryStorage()
  expect(readDismissed(storage)).toBe(false)
  writeDismissed(true, storage)
  expect(storage.getItem(DISMISS_KEY)).toBe("1")
  expect(readDismissed(storage)).toBe(true)
  writeDismissed(false, storage)
  expect(storage.getItem(DISMISS_KEY)).toBeNull()
  expect(readDismissed(storage)).toBe(false)
})

test("a storage that throws never breaks the page and the dismissal still holds in memory", () => {
  const broken = {
    getItem: () => {
      throw new Error("blocked")
    },
    setItem: () => {
      throw new Error("blocked")
    },
    removeItem: () => {
      throw new Error("blocked")
    }
  }
  expect(readDismissed(broken)).toBe(false)
  expect(() => writeDismissed(true, broken)).not.toThrow()
  expect(readDismissed(broken)).toBe(true)
  expect(() => writeDismissed(false, broken)).not.toThrow()
  expect(readDismissed(broken)).toBe(false)
})

test("the sample-loaded announcement names step 4, the only step that links to the workbook after loading", () => {
  const after = make(["data"], "sample-1")
  const stepNumber = buildSteps(after).findIndex((s) => s.actions.some((a) => a.kind === "link" && a.to === "/workbooks/sample-1")) + 1
  expect(stepNumber).toBe(4)
  expect(sampleLoadedMessage(true)).toBe("Sample workbook loaded. Run it from step 4.")
  expect(sampleLoadedMessage(false)).toBe("Sample workbook already loaded. Run it from step 4.")
})
