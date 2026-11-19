const ROLE_LABELS: Record<string, string> = {
  global_admin: "Instance admin",
  workspace_admin: "Workspace admin"
}

/** Human label for an API role string ("global_admin" -> "Instance admin"); unknown roles are sentence-cased. */
export function roleLabel(role?: string | null): string {
  if (!role) return ""
  const known = ROLE_LABELS[role.toLowerCase()]
  if (known) return known
  const words = role.replace(/[_-]+/g, " ").trim().toLowerCase()
  return words.charAt(0).toUpperCase() + words.slice(1)
}
