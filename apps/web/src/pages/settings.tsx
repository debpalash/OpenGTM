import { useState, useEffect } from "react"
import { toast } from "sonner"
import {
  ExternalLink, Check, X, Loader2, TestTube2,
  Eye, EyeOff, Star, Globe, Diamond, Leaf, Zap,
  Brain, Sparkles, Shell, Hexagon, Cloud, Smile, Flame, Waves,
  Search, Bot, BarChart3, Radio, Mail, ShieldCheck, Download, RefreshCw,
  Database, Trash2, UserPlus, LockKeyhole, Copy,
} from "lucide-react"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { useProviders, type Provider } from "@/lib/hooks"
import { useQueryClient } from "@tanstack/react-query"
import { queryKeys } from "@/lib/query-client"
import { useSmtpStatus, useUpdateSmtp, useTestSmtp } from "@/lib/automation-hooks"
import { Gate } from "@/components/gate"

// Map provider emoji icons from the API to Lucide components
const PROVIDER_ICON_MAP: Record<string, React.ComponentType<{ className?: string }>> = {
  "🌐": Globe,
  "🔷": Diamond,
  "💚": Leaf,
  "⚡": Zap,
  "🧠": Brain,
  "🔮": Sparkles,
  "🐚": Shell,
  "🐙": Hexagon,
  "☁️": Cloud,
  "🤗": Smile,
  "🔥": Flame,
  "🌊": Waves,
  "🎯": Search,
  "🚀": Zap,
  "📧": Mail,
  "📱": Radio,
  "🌍": Globe,
}

function ProviderIcon({ icon }: { icon: string }) {
  const Icon = PROVIDER_ICON_MAP[icon] || Globe
  return <Icon className="size-5 text-muted-foreground" />
}

function ProviderCard({ provider }: { provider: Provider }) {
  const [apiKey, setApiKey] = useState("")
  const [model, setModel] = useState(provider.model)
  const [showKey, setShowKey] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const qc = useQueryClient()

  const handleTest = async () => {
    setTesting(true)
    setTestResult(null)
    try {
      const res = await fetch(`/api/settings/providers/${provider.id}/test`, { method: "POST" })
      const data = await res.json()
      setTestResult(data.status === "ok" ? `✓ ${data.response}` : `✗ ${data.error}`)
    } catch (e) {
      setTestResult("✗ Network error")
    }
    setTesting(false)
  }

  const handleSave = async () => {
    setSaving(true)
    try {
      const body: Record<string, unknown> = {}
      if (apiKey) body.api_key = apiKey
      if (model !== provider.model) body.model = model
      await fetch(`/api/settings/providers/${provider.id}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
      toast.success(`${provider.name} updated`)
      qc.invalidateQueries({ queryKey: queryKeys.providers })
      setApiKey("")
    } catch {
      toast.error("Failed to save")
    }
    setSaving(false)
  }

  const handleSetDefault = async () => {
    await fetch(`/api/settings/providers/${provider.id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ set_default: true }),
    })
    toast.success(`${provider.name} set as default`)
    qc.invalidateQueries({ queryKey: queryKeys.providers })
  }

  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between">
          <CardTitle className="text-sm font-medium flex items-center gap-2">
            <ProviderIcon icon={provider.icon} />
            {provider.name}
            {provider.is_default && (
              <Badge variant="default" className="text-xs gap-1">
                <Star className="size-3" /> Default
              </Badge>
            )}
          </CardTitle>
          <div className="flex items-center gap-1">
            {provider.configured ? (
              <Badge variant="outline" className="text-xs text-green-600 border-green-600/20">
                <Check className="size-3 mr-1" /> Configured
              </Badge>
            ) : (
              <Badge variant="outline" className="text-xs text-muted-foreground">
                <X className="size-3 mr-1" /> Not set
              </Badge>
            )}
          </div>
        </div>
        <CardDescription className="text-xs">
          {provider.free_tier}
          <a href={provider.docs_url} target="_blank" rel="noopener noreferrer" className="ml-2 inline-flex items-center gap-0.5 hover:underline">
            Docs <ExternalLink className="size-3" />
          </a>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="space-y-1.5">
          <Label className="text-xs">API Key</Label>
          <div className="flex items-center gap-1">
            <Input
              type={showKey ? "text" : "password"}
              placeholder={provider.api_key_masked || "Enter API key..."}
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              className="font-mono text-xs"
            />
            <Button variant="ghost" size="sm" onClick={() => setShowKey(!showKey)}>
              {showKey ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
            </Button>
          </div>
        </div>

        <div className="space-y-1.5">
          <Label className="text-xs">Model</Label>
          <Input
            value={model}
            onChange={(e) => setModel(e.target.value)}
            placeholder={provider.default_model}
            className="text-xs"
          />
        </div>

        <div className="flex items-center gap-2 pt-1">
          <Button size="sm" onClick={handleSave} disabled={saving || (!apiKey && model === provider.model)}>
            {saving ? <Loader2 className="size-3 animate-spin" /> : "Save"}
          </Button>
          <Button size="sm" variant="outline" onClick={handleTest} disabled={testing || !provider.configured}>
            {testing ? <Loader2 className="size-3 animate-spin" /> : <><TestTube2 className="size-3" /> Test</>}
          </Button>
          {!provider.is_default && provider.configured && (
            <Button size="sm" variant="ghost" onClick={handleSetDefault}>
              Set default
            </Button>
          )}
        </div>

        {testResult && (
          <div className={`text-xs p-2 rounded ${testResult.startsWith("✓") ? "bg-green-500/10 text-green-600" : "bg-destructive/10 text-destructive"}`}>
            {testResult}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

export default function SettingsPage() {
  const { data, isLoading } = useProviders()

  return (
    <div className="p-6 space-y-6">
      <Tabs defaultValue="providers">
        <TabsList>
          <TabsTrigger value="providers">AI Providers</TabsTrigger>
          <TabsTrigger value="enrichment">Enrichment</TabsTrigger>
          <TabsTrigger value="email">Email</TabsTrigger>
          <TabsTrigger value="agents">Agents</TabsTrigger>
          <TabsTrigger value="pipeline">Pipeline</TabsTrigger>
          <TabsTrigger value="integrations">Integrations</TabsTrigger>
          <TabsTrigger value="governance">Governance</TabsTrigger>
        </TabsList>

        <TabsContent value="providers" className="mt-4 space-y-4">
          <div>
            <h3 className="text-sm font-medium">LLM Providers</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Configure AI models for lead extraction, scoring, and outreach.
              The default provider is used for all AI pipeline stages.
            </p>
          </div>
          <Separator />
          {isLoading ? (
            <div className="grid gap-4 md:grid-cols-2">
              {Array.from({ length: 6 }).map((_, i) => <Skeleton key={i} className="h-48" />)}
            </div>
          ) : (
            <div className="grid gap-4 md:grid-cols-2">
              {data?.providers.map(p => (
                <ProviderCard key={p.id} provider={p} />
              ))}
            </div>
          )}
        </TabsContent>

        <TabsContent value="enrichment" className="mt-4 space-y-4">
          <EnrichmentProvidersTab />
        </TabsContent>

        <TabsContent value="agents" className="mt-4 space-y-4">
          <div>
            <h3 className="text-sm font-medium">Agent Configuration</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Control which AI agents are active in the lead pipeline. Disabled agents will be skipped during collection.
            </p>
          </div>
          <Separator />
          <div className="grid gap-4 md:grid-cols-2">
            {[
              { id: "source_agent", name: "Source Agent", Icon: Search, desc: "Searches DDG, Maps, and directories for company URLs", default: true },
              { id: "enrichment_agent", name: "Enrichment Agent", Icon: Bot, desc: "Extracts company data, emails, phones from websites using AI", default: true },
              { id: "scoring_agent", name: "Scoring Agent", Icon: BarChart3, desc: "Scores leads against your ICP using LLM reasoning", default: true },
              { id: "signal_agent", name: "Signal Agent", Icon: Radio, desc: "Monitors hiring, funding, and growth signals", default: false },
              { id: "outreach_agent", name: "Outreach Agent", Icon: Mail, desc: "Generates and sends personalized outreach messages", default: false },
            ].map((agent) => (
              <Card key={agent.id}>
                <CardHeader className="pb-3">
                  <div className="flex items-center justify-between">
                    <CardTitle className="text-sm font-medium flex items-center gap-2">
                      <agent.Icon className="size-5 text-muted-foreground" />
                      {agent.name}
                    </CardTitle>
                    <div className="flex items-center gap-2">
                      <input
                        type="checkbox"
                        id={`agent-${agent.id}`}
                        defaultChecked={agent.default}
                        className="h-4 w-4 rounded border-gray-300"
                      />
                    </div>
                  </div>
                  <CardDescription className="text-xs">
                    {agent.desc}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <Badge variant={agent.default ? "outline" : "secondary"} className="text-xs">
                    {agent.default ? "Active" : "Idle"}
                  </Badge>
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="pipeline" className="mt-4 space-y-4">
          <div>
            <h3 className="text-sm font-medium">Enrichment Performance</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Tune parallelism and retries. Higher concurrency/workers = faster (more CPU/RAM);
              more retry passes = higher fill rate.
            </p>
          </div>
          <Separator />
          <EnrichmentPerfTab />
        </TabsContent>

        <TabsContent value="email" className="mt-4 space-y-4">
          <SMTPConfigTab />
        </TabsContent>

        <TabsContent value="integrations" className="mt-4 space-y-4">
          <div>
            <h3 className="text-sm font-medium">Integrations</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Connect CRM and export destinations used by workbook output columns.
            </p>
          </div>
          <Separator />
          <IntegrationsTab />
        </TabsContent>

        <TabsContent value="governance" className="mt-4 space-y-4">
          <GovernanceAuditTab />
        </TabsContent>
      </Tabs>
    </div>
  )
}

interface AuditEvent {
  id: string; actor_user_id: number | null; actor_role: string; method: string
  route: string; resource_path: string; response_status: number; outcome: string
  request_id: string; created_at: string
}

type RetentionCategory = "audit" | "signals" | "activation" | "audience_history" | "agent_results" | "outreach_history"
interface RetentionPolicy { enabled: boolean; legal_hold: boolean; retention_days: Record<RetentionCategory, number>; next_run_at: string | null }
interface RetentionRun { id: string; status: string; requested_by: string | null; deleted_counts: Record<string, number>; error: string | null; created_at: string }
const RETENTION_FIELDS: { key: RetentionCategory; label: string; hint: string; min: number }[] = [
  { key: "audit", label: "Audit events", hint: "Security and mutation evidence", min: 90 },
  { key: "signals", label: "Signal observations", hint: "Intent and market signal rows", min: 30 },
  { key: "activation", label: "Activation deliveries", hint: "Destination delivery history", min: 30 },
  { key: "audience_history", label: "Audience history", hint: "Membership change events", min: 30 },
  { key: "agent_results", label: "Agent results", hint: "Research playbook results", min: 30 },
  { key: "outreach_history", label: "Outreach history", hint: "Email send records", min: 30 },
]

function GovernanceAuditTab() {
  const [events, setEvents] = useState<AuditEvent[]>([])
  const [loading, setLoading] = useState(true)
  const load = () => {
    setLoading(true)
    fetch("/api/governance/audit-events?limit=100").then(async response => {
      if (!response.ok) throw new Error(response.status === 403 ? "Workspace admin access is required" : `Could not load audit log (${response.status})`)
      return response.json()
    }).then(data => setEvents(data.events || [])).catch(error => toast.error(error.message)).finally(() => setLoading(false))
  }
  useEffect(load, [])
  const download = async () => {
    const response = await fetch("/api/governance/audit-events/export.csv")
    if (!response.ok) { toast.error(`Could not export audit log (${response.status})`); return }
    const url = URL.createObjectURL(await response.blob())
    const anchor = document.createElement("a"); anchor.href = url; anchor.download = "opengtm-audit-events.csv"; anchor.click(); URL.revokeObjectURL(url)
  }
  return <>
    <RetentionPolicyCard />
    <Separator />
    <SsoPolicyCard />
    <Separator />
    <ScimProvisioningCard />
    <Separator />
    <WorkspaceAccessCard />
    <Separator />
    <div className="flex items-start justify-between gap-3"><div><h3 className="flex items-center gap-2 text-sm font-medium"><ShieldCheck className="size-4" /> Workspace audit log</h3><p className="mt-1 text-xs text-muted-foreground">Append-only records for authenticated API mutations. Request bodies and credentials are never retained.</p></div><div className="flex gap-2"><Button size="sm" variant="outline" onClick={load} disabled={loading}><RefreshCw className={loading ? "size-3 animate-spin" : "size-3"} /> Refresh</Button><Button size="sm" variant="outline" onClick={download}><Download className="size-3" /> Export CSV</Button></div></div>
    <Separator />
    <Card><CardContent className="p-0"><div className="max-h-[520px] overflow-auto"><table className="w-full text-left text-xs"><thead className="sticky top-0 bg-card text-muted-foreground"><tr><th className="p-3">Time</th><th className="p-3">Actor</th><th className="p-3">Action</th><th className="p-3">Resource</th><th className="p-3">Outcome</th><th className="p-3">Request ID</th></tr></thead><tbody>{events.map(event => <tr key={event.id} className="border-t"><td className="whitespace-nowrap p-3">{new Date(event.created_at).toLocaleString()}</td><td className="p-3">{event.actor_user_id ?? "system"} <span className="text-muted-foreground">({event.actor_role || "—"})</span></td><td className="p-3 font-mono">{event.method} {event.route}</td><td className="max-w-64 truncate p-3 font-mono text-muted-foreground">{event.resource_path}</td><td className="p-3"><Badge variant={event.outcome === "success" ? "outline" : "destructive"}>{event.response_status} {event.outcome}</Badge></td><td className="max-w-36 truncate p-3 font-mono text-muted-foreground" title={event.request_id}>{event.request_id}</td></tr>)}</tbody></table>{!loading && !events.length && <p className="p-10 text-center text-xs text-muted-foreground">No workspace mutations recorded yet.</p>}{loading && <div className="p-4"><Skeleton className="h-28 w-full" /></div>}</div></CardContent></Card>
  </>
}

interface RbacPermission { key: string; label: string; description: string; default_roles: string[] }
interface RbacMember { user_id: number; username: string; role: string; overrides: Record<string, "allow" | "deny"> }

interface SsoPolicy { enabled: boolean; enforce_sso: boolean; owner_password_fallback: boolean; issuer: string; client_id: string; client_secret?: string; client_secret_configured: boolean; allowed_domains: string[]; auto_provision: boolean; default_role: "viewer" | "member" | "editor" }

function SsoPolicyCard() {
  const [policy, setPolicy] = useState<SsoPolicy | null>(null)
  const [domains, setDomains] = useState("")
  const [saving, setSaving] = useState(false)
  useEffect(() => { fetch("/api/governance/sso").then(async response => { if (!response.ok) throw new Error("Could not load SSO policy"); return response.json() }).then(data => { setPolicy(data); setDomains((data.allowed_domains || []).join(", ")) }).catch(error => toast.error(error.message)) }, [])
  const save = async () => {
    if (!policy) return
    setSaving(true)
    try {
      const response = await fetch("/api/governance/sso", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ...policy, allowed_domains: domains.split(",").map(item => item.trim()).filter(Boolean) }) })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not save SSO policy")
      const data = await response.json(); setPolicy({ ...data, client_secret: "" }); setDomains((data.allowed_domains || []).join(", ")); toast.success("OIDC SSO policy saved")
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not save SSO policy") }
    finally { setSaving(false) }
  }
  if (!policy) return <Skeleton className="h-52 w-full" />
  return <Card><CardHeader><div className="flex items-start justify-between gap-3"><div><CardTitle className="flex items-center gap-2 text-sm"><LockKeyhole className="size-4" /> OpenID Connect SSO</CardTitle><CardDescription>Connect this workspace to an operator-approved OIDC provider. ID tokens require a verified email and are bound by issuer and subject.</CardDescription></div><Badge variant={policy.enforce_sso ? "default" : policy.enabled ? "outline" : "secondary"}>{policy.enforce_sso ? "Required" : policy.enabled ? "Optional" : "Disabled"}</Badge></div></CardHeader><CardContent className="space-y-4"><div className="flex flex-wrap gap-4"><label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={policy.enabled} onChange={event => setPolicy({ ...policy, enabled: event.target.checked, enforce_sso: event.target.checked ? policy.enforce_sso : false })} className="size-4" /> Enable SSO</label><label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={policy.enforce_sso} disabled={!policy.enabled} onChange={event => setPolicy({ ...policy, enforce_sso: event.target.checked })} className="size-4" /> Require SSO for members</label></div><div className="grid gap-3 md:grid-cols-2"><div className="space-y-1"><Label className="text-xs">Issuer URL</Label><Input value={policy.issuer} onChange={event => setPolicy({ ...policy, issuer: event.target.value })} placeholder="https://id.example.com" /></div><div className="space-y-1"><Label className="text-xs">Client ID</Label><Input value={policy.client_id} onChange={event => setPolicy({ ...policy, client_id: event.target.value })} /></div><div className="space-y-1"><Label className="text-xs">Client secret</Label><Input type="password" value={policy.client_secret || ""} onChange={event => setPolicy({ ...policy, client_secret: event.target.value })} placeholder={policy.client_secret_configured ? "Configured — leave blank to retain" : "Required when enabled"} /></div><div className="space-y-1"><Label className="text-xs">Allowed email domains</Label><Input value={domains} onChange={event => setDomains(event.target.value)} placeholder="example.com, subsidiary.com" /></div></div><div className="flex flex-wrap items-center gap-4"><label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={policy.auto_provision} onChange={event => setPolicy({ ...policy, auto_provision: event.target.checked })} className="size-4" /> Just-in-time provisioning</label><Label className="text-xs">Default role</Label><select aria-label="OIDC default role" value={policy.default_role} onChange={event => setPolicy({ ...policy, default_role: event.target.value as SsoPolicy["default_role"] })} className="h-9 rounded-md border bg-background px-3 text-xs"><option value="viewer">Viewer</option><option value="member">Member</option><option value="editor">Editor</option></select></div><p className="text-[11px] text-muted-foreground">Callback URL: <code>/auth/sso/&lt;workspace-slug&gt;/callback</code>. Allow the issuer hostname with <code>SSO_ALLOWED_ISSUER_HOSTS</code>. Workspace owners retain password access as an emergency recovery path.</p><Button size="sm" onClick={save} disabled={saving}>{saving && <Loader2 className="size-3 animate-spin" />} Save SSO policy</Button></CardContent></Card>
}

function ScimProvisioningCard() {
  const [status, setStatus] = useState<{ configured: boolean; base_path: string; token_prefix?: string; created_at?: number } | null>(null)
  const [token, setToken] = useState("")
  const [busy, setBusy] = useState(false)
  const load = () => fetch("/api/governance/scim-token").then(async response => { if (!response.ok) throw new Error("Could not load SCIM configuration"); setStatus(await response.json()) })
  useEffect(() => { load().catch(error => toast.error(error.message)) }, [])
  const rotate = async () => { setBusy(true); try { const response = await fetch("/api/governance/scim-token", { method: "POST" }); if (!response.ok) throw new Error("Could not rotate SCIM token"); const data = await response.json(); setToken(data.token); await load(); toast.success("SCIM token rotated") } catch (error) { toast.error(error instanceof Error ? error.message : "Could not rotate token") } finally { setBusy(false) } }
  const revoke = async () => { if (!window.confirm("Revoke the current SCIM token? Directory sync will stop immediately.")) return; setBusy(true); try { const response = await fetch("/api/governance/scim-token", { method: "DELETE" }); if (!response.ok) throw new Error("Could not revoke SCIM token"); setToken(""); await load(); toast.success("SCIM token revoked") } catch (error) { toast.error(error instanceof Error ? error.message : "Could not revoke token") } finally { setBusy(false) } }
  if (!status) return <Skeleton className="h-40 w-full" />
  return <Card><CardHeader><div className="flex items-start justify-between gap-3"><div><CardTitle className="flex items-center gap-2 text-sm"><UserPlus className="size-4" /> SCIM 2.0 provisioning</CardTitle><CardDescription>Provision and deactivate workspace members from your identity directory. The bearer token is workspace-scoped and shown only once.</CardDescription></div><Badge variant={status.configured ? "default" : "secondary"}>{status.configured ? "Configured" : "Disabled"}</Badge></div></CardHeader><CardContent className="space-y-3"><div className="rounded-md border bg-muted/20 p-3 text-xs"><span className="text-muted-foreground">Base URL</span><code className="mt-1 block">{window.location.origin}{status.base_path}</code>{status.token_prefix && <p className="mt-2 text-muted-foreground">Current token starts with <code>{status.token_prefix}…</code>{status.created_at ? ` · rotated ${new Date(status.created_at * 1000).toLocaleString()}` : ""}</p>}</div>{token && <div className="rounded-md border border-amber-400/40 bg-amber-50 p-3 text-xs text-amber-950"><strong>Copy this token now. It cannot be retrieved again.</strong><div className="mt-2 flex gap-2"><Input readOnly value={token} className="font-mono" /><Button size="icon" variant="outline" onClick={() => navigator.clipboard.writeText(token).then(() => toast.success("Token copied"))}><Copy className="size-3" /></Button></div></div>}<div className="flex gap-2"><Button size="sm" onClick={rotate} disabled={busy}>{busy && <Loader2 className="size-3 animate-spin" />}{status.configured ? "Rotate token" : "Create token"}</Button>{status.configured && <Button size="sm" variant="destructive" onClick={revoke} disabled={busy}>Revoke</Button>}</div></CardContent></Card>
}

function WorkspaceAccessCard() {
  const [data, setData] = useState<{ permissions: RbacPermission[]; members: RbacMember[] } | null>(null)
  const [saving, setSaving] = useState("")
  const [username, setUsername] = useState("")
  const [newRole, setNewRole] = useState("viewer")
  const load = () => fetch("/api/governance/rbac").then(async response => {
    if (!response.ok) throw new Error(response.status === 403 ? "Workspace admin access is required" : "Could not load access policy")
    setData(await response.json())
  })
  useEffect(() => { load().catch(error => toast.error(error.message)) }, [])
  const update = async (member: RbacMember, permission: RbacPermission, effect: string) => {
    const key = `${member.user_id}:${permission.key}`; setSaving(key)
    try {
      const response = await fetch(`/api/governance/rbac/${member.user_id}/${permission.key}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ effect: effect === "default" ? null : effect }) })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not update permission")
      await load(); toast.success(`${permission.label} access updated for ${member.username}`)
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not update permission") }
    finally { setSaving("") }
  }
  const addMember = async () => {
    if (!username.trim()) return
    setSaving("add")
    try {
      const response = await fetch("/api/governance/members", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: username.trim(), role: newRole }) })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not add member")
      setUsername(""); await load(); toast.success("Workspace member added")
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not add member") }
    finally { setSaving("") }
  }
  const changeRole = async (member: RbacMember, role: string) => {
    setSaving(`role:${member.user_id}`)
    try {
      const response = await fetch(`/api/governance/members/${member.user_id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ role }) })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not update role")
      await load(); toast.success(`${member.username} is now ${role}`)
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not update role") }
    finally { setSaving("") }
  }
  const removeMember = async (member: RbacMember) => {
    if (!window.confirm(`Remove ${member.username} from this workspace?`)) return
    setSaving(`remove:${member.user_id}`)
    try {
      const response = await fetch(`/api/governance/members/${member.user_id}`, { method: "DELETE" })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not remove member")
      await load(); toast.success(`${member.username} removed`)
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not remove member") }
    finally { setSaving("") }
  }
  if (!data) return <Skeleton className="h-64 w-full" />
  return <Card><CardHeader><CardTitle className="flex items-center gap-2 text-sm"><ShieldCheck className="size-4" /> Workspace members and capability policy</CardTitle><CardDescription>Provision existing OpenGTM users, manage their role, and override its capability baseline. Owners always retain full access.</CardDescription><div className="flex flex-col gap-2 pt-3 sm:flex-row"><Input aria-label="Username to add" placeholder="Existing username" value={username} onChange={event => setUsername(event.target.value)} onKeyDown={event => { if (event.key === "Enter") addMember() }} /><select aria-label="New member role" value={newRole} onChange={event => setNewRole(event.target.value)} className="h-9 rounded-md border bg-background px-3 text-xs"><option value="viewer">Viewer</option><option value="member">Member</option><option value="editor">Editor</option><option value="admin">Admin</option></select><Button size="sm" onClick={addMember} disabled={!username.trim() || saving === "add"}>{saving === "add" ? <Loader2 className="size-3 animate-spin" /> : <UserPlus className="size-3" />} Add member</Button></div></CardHeader><CardContent className="overflow-x-auto p-0"><table className="w-full min-w-[980px] text-left text-xs"><thead className="border-y bg-muted/30"><tr><th className="p-3">Member</th>{data.permissions.map(permission => <th key={permission.key} className="p-3"><span className="block">{permission.label}</span><span className="font-normal text-[9px] text-muted-foreground">{permission.default_roles.join(", ")}</span></th>)}</tr></thead><tbody>{data.members.map(member => <tr key={member.user_id} className="border-b"><td className="p-3"><span className="block font-medium">{member.username}</span><div className="mt-1 flex items-center gap-1">{member.role === "owner" ? <Badge variant="outline" className="text-[9px]">owner</Badge> : <><select aria-label={`Role for ${member.username}`} value={member.role} disabled={saving === `role:${member.user_id}`} onChange={event => changeRole(member, event.target.value)} className="h-7 rounded-md border bg-background px-1 text-[9px]"><option value="viewer">Viewer</option><option value="member">Member</option><option value="editor">Editor</option><option value="admin">Admin</option></select><Button aria-label={`Remove ${member.username}`} title={`Remove ${member.username}`} size="icon" variant="ghost" className="size-7 text-destructive" disabled={saving === `remove:${member.user_id}`} onClick={() => removeMember(member)}><Trash2 className="size-3" /></Button></>}</div></td>{data.permissions.map(permission => { const value = member.overrides[permission.key] || "default"; const key = `${member.user_id}:${permission.key}`; return <td key={permission.key} className="p-2"><select aria-label={`${permission.label} for ${member.username}`} title={permission.description} value={value} disabled={member.role === "owner" || saving === key} onChange={event => update(member, permission, event.target.value)} className={`h-8 w-full rounded-md border bg-background px-2 text-[10px] ${value === "deny" ? "text-destructive" : value === "allow" ? "text-emerald-600" : ""}`}><option value="default">Role default</option><option value="allow">Allow</option><option value="deny">Deny</option></select></td>})}</tr>)}</tbody></table></CardContent></Card>
}

function RetentionPolicyCard() {
  const [policy, setPolicy] = useState<RetentionPolicy | null>(null)
  const [runs, setRuns] = useState<RetentionRun[]>([])
  const [preview, setPreview] = useState<Record<string, number> | null>(null)
  const [confirmation, setConfirmation] = useState("")
  const [busy, setBusy] = useState("")

  const load = async () => {
    const [policyResponse, runsResponse] = await Promise.all([
      fetch("/api/governance/retention"), fetch("/api/governance/retention/runs"),
    ])
    if (!policyResponse.ok || !runsResponse.ok) throw new Error(policyResponse.status === 403 ? "Workspace admin access is required" : "Could not load retention policy")
    setPolicy(await policyResponse.json()); setRuns(await runsResponse.json())
  }
  useEffect(() => { load().catch(error => toast.error(error.message)) }, [])

  const save = async () => {
    if (!policy) return
    setBusy("save")
    try {
      const response = await fetch("/api/governance/retention", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(policy) })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not save retention policy")
      setPolicy(await response.json()); setPreview(null); toast.success("Retention policy saved")
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not save retention policy") }
    finally { setBusy("") }
  }
  const inspect = async () => {
    setBusy("preview")
    try {
      const response = await fetch("/api/governance/retention/preview", { method: "POST" })
      if (!response.ok) throw new Error("Could not preview expired records")
      setPreview((await response.json()).expired_counts)
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not preview expired records") }
    finally { setBusy("") }
  }
  const enforce = async () => {
    setBusy("enforce")
    try {
      const response = await fetch("/api/governance/retention/enforce", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ confirmation }) })
      if (!response.ok) throw new Error((await response.json()).detail || "Could not start retention run")
      const run = await response.json(); setRuns(current => [run, ...current]); setConfirmation(""); toast.success("Retention run queued")
    } catch (error) { toast.error(error instanceof Error ? error.message : "Could not start retention run") }
    finally { setBusy("") }
  }
  if (!policy) return <Skeleton className="h-64 w-full" />
  const expiredTotal = Object.values(preview || {}).reduce((sum, count) => sum + count, 0)
  return <Card>
    <CardHeader><div className="flex items-start justify-between gap-4"><div><CardTitle className="flex items-center gap-2 text-sm"><Database className="size-4" /> Data retention</CardTitle><CardDescription className="mt-1">Automatically remove expired operational history by workspace. Legal hold suspends every scheduled and manual purge.</CardDescription></div><Badge variant={policy.legal_hold ? "destructive" : policy.enabled ? "default" : "secondary"}>{policy.legal_hold ? "Legal hold" : policy.enabled ? "Scheduled" : "Manual only"}</Badge></div></CardHeader>
    <CardContent className="space-y-5">
      <div className="flex flex-wrap gap-6 rounded-md border p-3 text-xs"><label className="flex items-center gap-2"><input type="checkbox" checked={policy.enabled} onChange={event => setPolicy({ ...policy, enabled: event.target.checked })} className="size-4" /> Run daily</label><label className="flex items-center gap-2 font-medium text-destructive"><input type="checkbox" checked={policy.legal_hold} onChange={event => setPolicy({ ...policy, legal_hold: event.target.checked })} className="size-4" /> Legal hold</label>{policy.next_run_at && <span className="text-muted-foreground">Next run {new Date(policy.next_run_at).toLocaleString()}</span>}</div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">{RETENTION_FIELDS.map(field => <div key={field.key} className="space-y-1"><Label className="text-xs">{field.label}</Label><Input type="number" min={field.min} max={3650} value={policy.retention_days[field.key]} onChange={event => setPolicy({ ...policy, retention_days: { ...policy.retention_days, [field.key]: Number(event.target.value) } })} /><p className="text-[11px] text-muted-foreground">{field.hint} · min {field.min} days</p></div>)}</div>
      <div className="flex flex-wrap gap-2"><Button size="sm" onClick={save} disabled={!!busy}>{busy === "save" && <Loader2 className="size-3 animate-spin" />} Save policy</Button><Button size="sm" variant="outline" onClick={inspect} disabled={!!busy}>{busy === "preview" ? <Loader2 className="size-3 animate-spin" /> : <Eye className="size-3" />} Preview expired data</Button></div>
      {preview && <div className="rounded-md border border-destructive/30 bg-destructive/5 p-3 space-y-3"><p className="text-xs"><strong>{expiredTotal.toLocaleString()} records</strong> currently match the saved policy. Preview does not delete data.</p><div className="flex flex-wrap gap-2">{RETENTION_FIELDS.map(field => <Badge key={field.key} variant="outline">{field.label}: {(preview[field.key] || 0).toLocaleString()}</Badge>)}</div><div className="flex flex-col gap-2 sm:flex-row"><Input aria-label="Purge confirmation" placeholder="Type PURGE EXPIRED DATA" value={confirmation} onChange={event => setConfirmation(event.target.value)} className="font-mono" /><Button variant="destructive" onClick={enforce} disabled={confirmation !== "PURGE EXPIRED DATA" || policy.legal_hold || !!busy || expiredTotal === 0}>{busy === "enforce" ? <Loader2 className="size-3 animate-spin" /> : <Trash2 className="size-3" />} Purge expired data</Button></div></div>}
      {!!runs.length && <div className="space-y-2"><Label className="text-xs">Recent enforcement runs</Label>{runs.slice(0, 5).map(run => <div key={run.id} className="flex items-center justify-between gap-3 rounded-md border px-3 py-2 text-xs"><span>{new Date(run.created_at).toLocaleString()} · {Object.values(run.deleted_counts || {}).reduce((sum, count) => sum + count, 0).toLocaleString()} deleted</span><Badge variant={run.status === "failed" ? "destructive" : "outline"}>{run.status}</Badge></div>)}</div>}
    </CardContent>
  </Card>
}

// ── Enrichment Performance Tab ───────────────────────────────────

interface EnrichmentPerf {
  row_concurrency: number
  provider_workers: number
  provider_timeout: number
  max_providers: number
  retry_passes: number
}

const PERF_FIELDS: { key: keyof EnrichmentPerf; label: string; hint: string; min: number; max: number }[] = [
  { key: "row_concurrency", label: "Row concurrency", hint: "Rows enriched in parallel", min: 1, max: 64 },
  { key: "provider_workers", label: "Provider workers", hint: "Killable subprocesses for provider calls", min: 1, max: 64 },
  { key: "provider_timeout", label: "Provider timeout (s)", hint: "Kill a provider after this many seconds", min: 3, max: 120 },
  { key: "max_providers", label: "Max providers / cell", hint: "Waterfall depth cap (0 = full chain)", min: 0, max: 20 },
  { key: "retry_passes", label: "Retry passes", hint: "Extra passes over cells still failing", min: 0, max: 5 },
]

function EnrichmentPerfTab() {
  const [perf, setPerf] = useState<EnrichmentPerf | null>(null)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    fetch("/api/settings/enrichment-perf")
      .then(r => r.json())
      .then(d => setPerf({
        row_concurrency: d.row_concurrency, provider_workers: d.provider_workers,
        provider_timeout: d.provider_timeout, max_providers: d.max_providers,
        retry_passes: d.retry_passes,
      }))
      .catch(() => {})
  }, [])

  async function save() {
    if (!perf) return
    setSaving(true)
    try {
      const res = await fetch("/api/settings/enrichment-perf", {
        method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(perf),
      })
      if (!res.ok) throw new Error("save failed")
      const d = await res.json()
      setPerf(d)
      toast.success("Enrichment settings saved — applies to the next run")
    } catch { toast.error("Failed to save") }
    finally { setSaving(false) }
  }

  if (!perf) return <Skeleton className="h-40 w-full" />

  return (
    <Card>
      <CardContent className="pt-6 space-y-4">
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          {PERF_FIELDS.map(f => (
            <div key={f.key} className="space-y-1">
              <Label className="text-xs">{f.label}</Label>
              <Input
                type="number" min={f.min} max={f.max} value={perf[f.key]}
                onChange={e => setPerf({ ...perf, [f.key]: Number(e.target.value) })}
              />
              <p className="text-[11px] text-muted-foreground">{f.hint}</p>
            </div>
          ))}
        </div>
        <Button onClick={save} disabled={saving} size="sm">
          {saving ? "Saving…" : "Save"}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── Enrichment Providers Tab ─────────────────────────────────────

interface EnrichmentProvider {
  id: string
  name: string
  icon: string
  capability: string
  configured: boolean
  api_key_masked: string
  free_tier: string
  docs_url: string
}

function EnrichmentProvidersTab() {
  const [providers, setProviders] = useState<EnrichmentProvider[]>([])
  const [loading, setLoading] = useState(true)

  const fetchProviders = async () => {
    try {
      const res = await fetch("/api/settings/enrichment-providers")
      const data = await res.json()
      setProviders(data.providers || [])
    } catch { /* ignore */ }
    setLoading(false)
  }

  useEffect(() => { fetchProviders() }, [])

  return (
    <>
      <div>
        <h3 className="text-sm font-medium">Enrichment API Keys (BYOK)</h3>
        <p className="text-xs text-muted-foreground mt-1">
          Configure third-party enrichment providers. These are used in workbook waterfall columns to find emails, phone numbers, and company data.
        </p>
      </div>
      <Separator />
      {loading ? (
        <div className="grid gap-4 md:grid-cols-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-36" />)}
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {providers.map(p => (
            <EnrichmentProviderCard key={p.id} provider={p} onSaved={fetchProviders} />
          ))}
        </div>
      )}
    </>
  )
}

function EnrichmentProviderCard({ provider, onSaved }: { provider: EnrichmentProvider; onSaved: () => void }) {
  const [apiKey, setApiKey] = useState("")
  const [showKey, setShowKey] = useState(false)
  const [saving, setSaving] = useState(false)

  const handleSave = async () => {
    if (!apiKey) return
    setSaving(true)
    try {
      await fetch(`/api/settings/enrichment-providers/${provider.id}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ api_key: apiKey }),
      })
      toast.success(`${provider.name} key saved`)
      setApiKey("")
      onSaved()
    } catch {
      toast.error("Failed to save")
    }
    setSaving(false)
  }

  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between">
          <CardTitle className="text-sm font-medium flex items-center gap-2">
            <ProviderIcon icon={provider.icon} />
            {provider.name}
          </CardTitle>
          <div className="flex items-center gap-1">
            {provider.configured ? (
              <Badge variant="outline" className="text-xs text-green-600 border-green-600/20">
                <Check className="size-3 mr-1" /> Configured
              </Badge>
            ) : (
              <Badge variant="outline" className="text-xs text-muted-foreground">
                <X className="size-3 mr-1" /> Not set
              </Badge>
            )}
          </div>
        </div>
        <CardDescription className="text-xs">
          {provider.capability} · {provider.free_tier}
          <a href={provider.docs_url} target="_blank" rel="noopener noreferrer" className="ml-2 inline-flex items-center gap-0.5 hover:underline">
            Docs <ExternalLink className="size-3" />
          </a>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="space-y-1.5">
          <Label className="text-xs">API Key</Label>
          <div className="flex items-center gap-1">
            <Input
              type={showKey ? "text" : "password"}
              placeholder={provider.api_key_masked || "Enter API key..."}
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              className="font-mono text-xs"
            />
            <Button variant="ghost" size="sm" onClick={() => setShowKey(!showKey)}>
              {showKey ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
            </Button>
          </div>
        </div>
        <div className="flex items-center gap-2 pt-1">
          <Button size="sm" onClick={handleSave} disabled={saving || !apiKey}>
            {saving ? <Loader2 className="size-3 animate-spin" /> : "Save"}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

// ── SMTP Config Tab ──────────────────────────────────────────────

// ── Integrations Tab (output destinations) ───────────────────────
interface IntegrationField {
  key: string; label: string; secret: boolean; placeholder: string
  set: boolean; value: string; masked: string
}
interface Integration {
  id: string; name: string; icon: string; description: string
  connected: boolean; fields: IntegrationField[]
}

function IntegrationsTab() {
  const [integrations, setIntegrations] = useState<Integration[]>([])
  const [loading, setLoading] = useState(true)
  const [edits, setEdits] = useState<Record<string, string>>({})
  const [savingId, setSavingId] = useState<string | null>(null)

  const load = () => {
    setLoading(true)
    fetch("/api/settings/integrations")
      .then(r => r.json())
      .then(d => {
        const list: Integration[] = d.integrations || []
        setIntegrations(list)
        // Seed non-secret values (e.g. instance URL) so they show in the field.
        const seed: Record<string, string> = {}
        for (const it of list) for (const f of it.fields) if (!f.secret && f.value) seed[f.key] = f.value
        setEdits(seed)
      })
      .catch(() => {})
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  const save = async (it: Integration) => {
    setSavingId(it.id)
    try {
      const values: Record<string, string> = {}
      for (const f of it.fields) {
        const v = edits[f.key]
        if (v !== undefined && v !== "") values[f.key] = v
      }
      const res = await fetch(`/api/settings/integrations/${it.id}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ values }),
      })
      if (!res.ok) throw new Error()
      toast.success(`${it.name} saved`)
      // Clear secret inputs (they're never echoed back).
      setEdits(prev => {
        const n = { ...prev }
        for (const f of it.fields) if (f.secret) delete n[f.key]
        return n
      })
      load()
    } catch {
      toast.error(`Failed to save ${it.name}`)
    } finally {
      setSavingId(null)
    }
  }

  if (loading) return <Skeleton className="h-40 w-full" />

  return (
    <div className="space-y-3">
      {integrations.map(it => (
        <Card key={it.id}>
          <CardHeader className="pb-3">
            <div className="flex items-center justify-between">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Globe className="size-4" />
                {it.name}
              </CardTitle>
              <Badge variant={it.connected ? "default" : "secondary"} className="text-xs">
                {it.connected ? "Connected" : "Not connected"}
              </Badge>
            </div>
            <CardDescription className="text-xs">{it.description}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-2">
            {it.fields.map(f => (
              <div key={f.key} className="space-y-1">
                <Label className="text-xs">
                  {f.label}
                  {f.set && f.secret && (
                    <span className="ml-2 text-[10px] text-muted-foreground">saved {f.masked}</span>
                  )}
                </Label>
                <Input
                  type={f.secret ? "password" : "text"}
                  value={edits[f.key] ?? ""}
                  placeholder={f.set && f.secret ? "•••••• (saved — leave blank to keep)" : f.placeholder}
                  onChange={e => setEdits(prev => ({ ...prev, [f.key]: e.target.value }))}
                  className="text-xs"
                />
              </div>
            ))}
            <Button size="sm" onClick={() => save(it)} disabled={savingId === it.id} className="mt-1">
              {savingId === it.id ? "Saving…" : "Save"}
            </Button>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function SMTPConfigTab() {
  // Rewired onto the scaffold React Query hooks (spec §1.1 / §10). Status reads
  // through useSmtpStatus; save/test go through useUpdateSmtp/useTestSmtp which
  // route 403 (admin-only) + 400 errors through the shared ApiError mapper.
  const status = useSmtpStatus()
  const update = useUpdateSmtp({ onSuccess: () => { toast.success("SMTP configuration saved"); setPassword("") } })
  const test = useTestSmtp({ onSuccess: () => toast.success(`Test email sent to ${testEmail}`) })

  const [host, setHost] = useState("")
  const [port, setPort] = useState("587")
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [fromName, setFromName] = useState("OpenGTM")
  const [maxPerHour, setMaxPerHour] = useState("50")
  const [testEmail, setTestEmail] = useState("")
  // Seed inputs from the loaded status exactly once per loaded payload.
  const [seeded, setSeeded] = useState(false)

  const configured = status.data?.configured ?? false

  useEffect(() => {
    if (seeded || !status.data) return
    const d = status.data
    if (d.host) setHost(d.host)
    if (d.email) setEmail(d.email)
    if (d.from_name) setFromName(d.from_name)
    if (d.max_per_hour) setMaxPerHour(String(d.max_per_hour))
    setSeeded(true)
  }, [status.data, seeded])

  const handleSave = () => {
    update.mutate({
      smtp_host: host,
      smtp_port: parseInt(port),
      smtp_email: email,
      ...(password && { smtp_password: password }),
      smtp_from_name: fromName,
      smtp_max_per_hour: parseInt(maxPerHour),
    })
  }

  const handleTest = () => {
    if (!testEmail) { toast.error("Enter test email address"); return }
    test.mutate(testEmail)
  }

  const saving = update.isPending
  const testing = test.isPending

  return (
    <>
      <div>
        <h3 className="text-sm font-medium">SMTP Configuration</h3>
        <p className="text-xs text-muted-foreground mt-1">
          Configure your email server for outreach sequences. Works with Gmail, SendGrid, Mailgun, or any SMTP provider.
        </p>
      </div>
      <Separator />
      <Card>
        <CardContent className="pt-6 space-y-4">
          <div className="grid gap-4 md:grid-cols-2">
            <div className="space-y-1.5">
              <Label className="text-xs">SMTP Host</Label>
              <Input
                placeholder="smtp.gmail.com"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                className="text-xs"
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Port</Label>
              <Input
                placeholder="587"
                value={port}
                onChange={(e) => setPort(e.target.value)}
                className="text-xs"
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Email Address</Label>
              <Input
                placeholder="you@company.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                className="text-xs"
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Password / App Password</Label>
              <Input
                type="password"
                placeholder={configured ? "••••••••" : "Enter password"}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                className="text-xs font-mono"
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">From Name</Label>
              <Input
                placeholder="OpenGTM"
                value={fromName}
                onChange={(e) => setFromName(e.target.value)}
                className="text-xs"
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Max Emails/Hour</Label>
              <Input
                type="number"
                placeholder="50"
                value={maxPerHour}
                onChange={(e) => setMaxPerHour(e.target.value)}
                className="text-xs"
              />
            </div>
          </div>

          <div className="flex items-center gap-2">
            <Gate need="admin">
              <Button size="sm" onClick={handleSave} disabled={saving || !host || !email}>
                {saving ? <Loader2 className="size-3 animate-spin mr-1" /> : null}
                Save
              </Button>
            </Gate>
          </div>

          <Separator />

          <div className="space-y-2">
            <Label className="text-xs">Test Email</Label>
            <div className="flex items-center gap-2">
              <Input
                placeholder="test@example.com"
                value={testEmail}
                onChange={(e) => setTestEmail(e.target.value)}
                className="text-xs max-w-xs"
              />
              <Gate need="admin">
                <Button size="sm" variant="outline" onClick={handleTest} disabled={testing || !configured}>
                  {testing ? <Loader2 className="size-3 animate-spin mr-1" /> : <Mail className="size-3 mr-1" />}
                  Send Test
                </Button>
              </Gate>
            </div>
            {!configured && (
              <p className="text-[11px] text-muted-foreground">Save SMTP config first to send test emails.</p>
            )}
          </div>
        </CardContent>
      </Card>
    </>
  )
}
