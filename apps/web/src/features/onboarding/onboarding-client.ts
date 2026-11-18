// Client for the first-run checklist: GET /api/onboarding (step state computed server-side from real
// workspace data) and POST /api/onboarding/sample (loads the sample workbook, idempotent).

export const STEP_IDS = ["llm_provider", "data_source", "data", "enrichment"] as const
export type StepId = (typeof STEP_IDS)[number]

export interface OnboardingState {
  steps: { id: StepId; done: boolean }[]
  done_count: number
  total: number
  complete: boolean
  sample_workbook_id: string | null
}

export interface SampleResult {
  workbook_id: string
  name: string
  created: boolean
}

export class OnboardingError extends Error {
  readonly status?: number
  constructor(message: string, status?: number) {
    super(message)
    this.name = "OnboardingError"
    this.status = status
  }
}

export function validateOnboardingState(value: unknown): OnboardingState {
  const data = value as OnboardingState | null
  const bad = () => new OnboardingError("The setup checklist could not be verified. Try again.")
  if (
    !data ||
    !Array.isArray(data.steps) ||
    typeof data.done_count !== "number" ||
    typeof data.total !== "number" ||
    typeof data.complete !== "boolean" ||
    !(data.sample_workbook_id === null || typeof data.sample_workbook_id === "string")
  )
    throw bad()
  const seen = new Set<string>()
  for (const step of data.steps) {
    if (
      !step ||
      !(STEP_IDS as readonly string[]).includes(step.id) ||
      seen.has(step.id) ||
      typeof step.done !== "boolean"
    )
      throw bad()
    seen.add(step.id)
  }
  if (data.steps.length !== data.total || data.steps.filter((s) => s.done).length !== data.done_count) throw bad()
  return data
}

export async function fetchOnboarding(signal?: AbortSignal): Promise<OnboardingState> {
  const response = await fetch("/api/onboarding", { signal })
  if (!response.ok)
    throw new OnboardingError(`The setup checklist could not be loaded (${response.status}). Try again.`, response.status)
  return validateOnboardingState(await response.json())
}

export function validateSampleResult(value: unknown): SampleResult {
  const data = value as SampleResult | null
  if (
    !data ||
    typeof data.workbook_id !== "string" ||
    !data.workbook_id ||
    typeof data.name !== "string" ||
    typeof data.created !== "boolean"
  )
    throw new OnboardingError("The sample workbook response could not be verified. Check your workbooks.")
  return data
}

export async function loadSampleWorkbook(signal?: AbortSignal): Promise<SampleResult> {
  const response = await fetch("/api/onboarding/sample", { method: "POST", signal })
  if (response.status === 403)
    throw new OnboardingError("Only editors and admins can load the sample workbook. Ask one to do it.", 403)
  if (response.status === 429)
    throw new OnboardingError("Too many attempts. Wait a minute and try again.", 429)
  if (!response.ok)
    throw new OnboardingError(`The sample workbook could not be loaded (${response.status}). Try again.`, response.status)
  return validateSampleResult(await response.json())
}
