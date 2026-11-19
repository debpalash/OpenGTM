// Toasts with a way forward. A message that tells the user to go somewhere
// ("add a key in Settings") gets a button that takes them to that exact place,
// so nobody has to hunt for it. Import `toast` from here, not from "sonner".
import { toast as sonner, type ExternalToast } from "sonner"

export interface NextStep { label: string; to: string }

// First match wins; put the specific rules above the general ones.
const RULES: Array<[RegExp, NextStep]> = [
  [/\b(ai|llm) provider|language model|rate limited by|no fallback providers|copilot chat/i,
    { label: "Open AI provider settings", to: "/settings?tab=providers" }],
  [/smtp|mailbox|sender (address|identity)|email (provider|account|settings)/i,
    { label: "Open email settings", to: "/settings?tab=email" }],
  [/hubspot|salesforce|airtable|google sheets|attio|heyreach|lemlist|smartlead|instantly|\bcrm\b|integration/i,
    { label: "Open integrations", to: "/settings?tab=integrations" }],
  [/needs an api key|provider_key_missing|enrichment provider|no_verifier|verifier/i,
    { label: "Open enrichment settings", to: "/settings?tab=enrichment" }],
  [/data source|no sources? (enabled|configured)/i, { label: "Open data sources", to: "/sources" }],
  [/api key|\bin settings\b|go to settings|settings →/i, { label: "Open settings", to: "/settings" }],
  [/permission|not allowed|forbidden|ask (an|your) (admin|owner)/i,
    { label: "View access", to: "/settings?tab=governance" }],
  [/session (has )?expired|sign in again|not authenticated/i, { label: "Sign in", to: "/login" }],
]

// Destinations the server named for error messages (X-Next-Step headers),
// newest last. The server knows the cause, so its answer beats the wording
// rules below. Capped so a long session cannot grow it without bound.
const fromServer = new Map<string, NextStep>()
const MAX_REMEMBERED = 50

/** Only same-site paths: a header must never send the user off-site. */
export function safeInternalPath(to: string | null | undefined): string | null {
  if (!to || !to.startsWith("/") || to.startsWith("//") || to.includes("\\")) return null
  return to
}

/** Records the page the server named for an error message. */
export function rememberNextStep(message: string, to: string | null, label: string | null) {
  const path = safeInternalPath(to)
  if (!message || !path) return
  fromServer.delete(message)
  fromServer.set(message, { label: label?.trim() || "Fix this", to: path })
  if (fromServer.size > MAX_REMEMBERED) fromServer.delete(fromServer.keys().next().value!)
}

/** The place a message points to, or null when it names none. */
export function nextStepFor(message: unknown): NextStep | null {
  if (typeof message !== "string" || !message) return null
  // Callers often wrap the server text ("Could not start: SMTP not configured").
  const known = [...fromServer].reverse().find(([detail]) => message.includes(detail))
  if (known) return known[1]
  for (const [pattern, step] of RULES) if (pattern.test(message)) return step
  return null
}

let navigate: (to: string) => void = (to) => window.location.assign(to)
/** App wires the router in once, so actions navigate without a page reload. */
export function setNavigator(fn: (to: string) => void) { navigate = fn }

type Message = Parameters<typeof sonner.error>[0]
const withStep = (message: Message, data?: ExternalToast): ExternalToast | undefined => {
  if (data?.action) return data
  const step = nextStepFor(message)
  if (!step) return data
  // A toast with an action stays long enough to be read and clicked.
  return { duration: 12_000, ...data, action: { label: step.label, onClick: () => navigate(step.to) } }
}

export const toast = Object.assign(
  (message: Message, data?: ExternalToast) => sonner(message, data),
  sonner,
  {
    error: (message: Message, data?: ExternalToast) => sonner.error(message, withStep(message, data)),
    warning: (message: Message, data?: ExternalToast) => sonner.warning(message, withStep(message, data)),
    info: (message: Message, data?: ExternalToast) => sonner.info(message, withStep(message, data)),
  },
)
