import type { AudienceDestination, DestinationType } from "@/lib/api"
import { recordAPI } from "@/lib/records"
import {
  validateDestinationList,
  type AudienceActivationContext
} from "./destination-client"

type Kind = AudienceDestination["destination_type"]
export interface SetupField {
  key: string
  label: string
  placeholder?: string
  help?: string
  initial?: string
  optional?: boolean
  multiline?: boolean
  choices?: { value: string; label: string }[]
}
const policy: SetupField = {
  key: "inbound_conflict_policy",
  label: "Inbound sync policy",
  initial: "fill_missing",
  choices: [
    { value: "fill_missing", label: "Fill missing fields" },
    { value: "crm_wins", label: "CRM wins" }
  ],
  help: "Applies to inbound callbacks. CRM wins can replace existing values; configuring a destination does not configure callbacks."
}
export const SETUP_TYPES: Record<
  Kind,
  { label: string; fields: SetupField[]; help: string }
> = {
  webhook: {
    label: "Webhook",
    fields: [
      {
        key: "url",
        label: "Webhook URL",
        placeholder: "https://…",
        help: "HTTP or HTTPS, without a username or password in the URL."
      }
    ],
    help: "Sends member snapshot fields as JSON using POST. No custom field map is configured here."
  },
  warehouse_http: {
    label: "Warehouse HTTP",
    fields: [
      {
        key: "url",
        label: "Ingestion endpoint",
        placeholder: "https://…",
        help: "HTTPS endpoint without embedded credentials."
      },
      {
        key: "header_secret_ref",
        label: "Workspace secret key",
        help: "Reference to an existing encrypted workspace secret; enter its key, not its value."
      },
      { key: "dataset", label: "Dataset", initial: "opengtm_audience" }
    ],
    help: "Exports a checksum-manifested JSONL snapshot per run. The referenced workspace secret must already exist."
  },
  hubspot: {
    label: "HubSpot",
    fields: [policy],
    help: "Uses the workspace HubSpot connection. Saving does not verify the connection or configure inbound callbacks."
  },
  salesforce: {
    label: "Salesforce",
    fields: [policy],
    help: "Uses the workspace Salesforce connection. Saving does not verify the connection or configure inbound callbacks."
  },
  instantly: {
    label: "Instantly",
    fields: [{ key: "campaign_id", label: "Campaign ID" }],
    help: "Adds eligible members to this campaign through the workspace connection and skips existing campaign members. This may enroll people in outbound sequences."
  },
  smartlead: {
    label: "Smartlead",
    fields: [{ key: "campaign_id", label: "Campaign ID" }],
    help: "Adds eligible members to this campaign through the workspace connection. This may enroll people in outbound sequences."
  },
  google_sheets: {
    label: "Google Sheets",
    fields: [
      { key: "spreadsheet_id", label: "Spreadsheet ID" },
      { key: "range", label: "Sheet range", initial: "Sheet1!A:ZZ" },
      {
        key: "columns",
        label: "Snapshot fields",
        initial: "company,email,phone,website",
        help: "Comma-separated field names, in output order; 1–100 fields. The first sheet column is reserved for the stable delivery key."
      }
    ],
    help: "Uses the workspace Google Sheets connection. The first sheet column is reserved for the stable delivery key; the selected fields follow in order. Retries update existing rows."
  },
  airtable: {
    label: "Airtable",
    fields: [
      { key: "base_id", label: "Base ID" },
      { key: "table", label: "Table name or ID" },
      {
        key: "idempotency_field",
        label: "Merge field",
        initial: "OpenGTM ID",
        help: "Create this field in Airtable first. Retries use an atomic upsert; typecasting is enabled."
      }
    ],
    help: "Uses the workspace Airtable connection and the named merge field for atomic upsert, with typecasting enabled."
  },
  slack: {
    label: "Slack",
    fields: [
      {
        key: "webhook_secret_ref",
        label: "Workspace secret key",
        help: "Key of a workspace secret containing the Slack incoming-webhook URL. Do not enter the URL here."
      },
      {
        key: "message_template",
        label: "Message template",
        initial: "New audience member: {company} {contact_person} {email}",
        multiline: true,
        optional: true,
        help: "Up to 2,000 characters. Use plain {field_name} placeholders; {{ and }} escape braces. Blank uses the default template."
      }
    ],
    help: "Posts a message using snapshot fields. The referenced webhook secret must already exist in this workspace."
  },
  meta_ads: {
    label: "Meta Ads",
    fields: [
      { key: "custom_audience_id", label: "Custom audience ID" },
      { key: "consent_source", label: "Consent source / policy" }
    ],
    help: "SHA-256 hashes supported identifiers before upload. Platform approval may be required; review advertising permission for this audience."
  },
  google_ads: {
    label: "Google Ads",
    fields: [
      { key: "customer_id", label: "Customer ID" },
      { key: "user_list_id", label: "User list ID" },
      { key: "consent_source", label: "Consent source / policy" }
    ],
    help: "SHA-256 hashes supported identifiers before upload. Platform approval may be required; review advertising permission for this audience."
  },
  linkedin_ads: {
    label: "LinkedIn Ads",
    fields: [
      { key: "segment_id", label: "Segment ID" },
      { key: "consent_source", label: "Consent source / policy" }
    ],
    help: "SHA-256 hashes supported identifiers before upload. Platform approval may be required; review advertising permission for this audience."
  }
}
export interface DestinationDraft {
  name: string
  type: Kind
  enabled: boolean
  configs: Partial<Record<Kind, Record<string, string>>>
}
export function initialDestinationDraft(): DestinationDraft {
  return { name: "", type: "webhook", enabled: true, configs: {} }
}
export function draftValues(draft: DestinationDraft): Record<string, string> {
  return Object.fromEntries(
    SETUP_TYPES[draft.type].fields.map((field) => [
      field.key,
      draft.configs[draft.type]?.[field.key] ?? field.initial ?? ""
    ])
  )
}
export interface CreateDestinationInput {
  audience_id: string
  name: string
  destination_type: Kind
  enabled: boolean
  config: Record<string, unknown>
  field_map: Record<string, string>
}
export class SetupValidationError extends Error {
  readonly fields: Record<string, string>
  constructor(fields: Record<string, string>) {
    super("Check the highlighted destination fields.")
    this.fields = fields
  }
}
export function destinationInput(
  audience: string,
  draft: DestinationDraft
): CreateDestinationInput {
  const values = draftValues(draft),
    config: Record<string, unknown> = {},
    errors: Record<string, string> = {}
  const name = draft.name.trim()
  if (!name || Array.from(name).length > 200)
    errors.name = "Use a destination name of 1–200 characters."
  for (const field of SETUP_TYPES[draft.type].fields) {
    const value = values[field.key]!.trim()
    if (!value && !field.optional)
      errors[field.key] = `${field.label} is required.`
    if (
      field.choices &&
      !field.choices.some((choice) => choice.value === value)
    )
      errors[field.key] = "Choose an available policy."
    config[field.key] = value
  }
  if ("url" in values && values.url!.trim()) {
    try {
      const url = new URL(values.url!.trim())
      if (
        !/^https?:\/\//i.test(values.url!.trim()) ||
        !url.hostname ||
        url.username ||
        url.password ||
        !["http:", "https:"].includes(url.protocol) ||
        (draft.type === "warehouse_http" && url.protocol !== "https:")
      )
        throw new Error()
    } catch {
      errors.url =
        draft.type === "warehouse_http"
          ? "Use an HTTPS URL without embedded credentials."
          : "Use an HTTP or HTTPS URL without embedded credentials."
    }
  }
  if (draft.type === "webhook") config.method = "POST"
  if (draft.type === "warehouse_http") config.mode = "snapshot"
  if (draft.type === "instantly") config.skip_if_in_campaign = true
  if (draft.type === "airtable") config.typecast = true
  if (draft.type === "google_sheets") {
    const columns = values
      .columns!.split(",")
      .map((v) => v.trim())
      .filter(Boolean)
    if (!columns.length || columns.length > 100)
      errors.columns = "Choose 1–100 snapshot fields."
    config.columns = columns
  }
  if (draft.type.endsWith("_ads")) {
    config.consent_attested = false
    for (const field of SETUP_TYPES[draft.type].fields.filter(
      (f) => f.key !== "consent_source"
    )) {
      const value = String(config[field.key])
      if (!/^[0-9A-Za-z_.-]{1,64}$/.test(value) || /^\.+$/.test(value))
        errors[field.key] =
          "Use 1–64 letters, numbers, underscores, dots or hyphens."
    }
  }
  if (draft.type === "slack") {
    const template = String(config.message_template)
    if (Array.from(template).length > 2000)
      errors.message_template = "Use at most 2,000 characters."
    const unescaped = template
      .replace(/\{\{|\}\}/g, "")
      .replace(/\{[A-Za-z_][A-Za-z0-9_]*\}/g, "")
    if (/[{}]/.test(unescaped))
      errors.message_template =
        "Use plain {field_name} placeholders or escape braces with {{ and }}."
  }
  if (Object.keys(errors).length) throw new SetupValidationError(errors)
  return {
    audience_id: audience,
    name,
    destination_type: draft.type,
    enabled: draft.enabled,
    config,
    field_map: {}
  }
}
const object = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v)
export {
  validateDestinationCatalog as validateSetupCatalog,
  fetchDestinationCatalog as fetchSetupCatalog
} from "./destination-catalog"
function stable(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(stable).join(",")}]`
  if (object(v))
    return `{${Object.keys(v)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${stable(v[k])}`)
      .join(",")}}`
  return JSON.stringify(v) ?? "null"
}
export function creationFingerprint(
  context: AudienceActivationContext,
  type: DestinationType,
  input: CreateDestinationInput
): string {
  const a = context.audience
  return stable({
    audience: {
      id: a.id,
      name: a.name,
      filters: a.filters,
      members: a.member_count,
      refreshed: a.refreshed_at,
      enabled: a.refresh_enabled,
      interval: a.refresh_interval_minutes
    },
    type,
    input
  })
}
export function sameDestination(
  saved: AudienceDestination,
  input: CreateDestinationInput
): boolean {
  return (
    saved.audience_id === input.audience_id &&
    saved.name === input.name &&
    saved.destination_type === input.destination_type &&
    saved.enabled === input.enabled &&
    stable(saved.config) === stable(input.config) &&
    stable(Object.entries(saved.field_map)) ===
      stable(Object.entries(input.field_map))
  )
}
export async function createReviewedDestination(
  input: CreateDestinationInput
): Promise<AudienceDestination> {
  if (
    input.destination_type.endsWith("_ads") &&
    input.config.consent_attested !== true
  )
    throw new Error(
      "Confirm advertising permission for the reviewed audience and target."
    )
  const value = await recordAPI<unknown>("audience-destinations", input, "POST")
  const saved = validateDestinationList([value], input.audience_id)[0]!
  if (!sameDestination(saved, input))
    throw new Error(
      "The saved destination did not confirm the reviewed configuration."
    )
  return saved
}
