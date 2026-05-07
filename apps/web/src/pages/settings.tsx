import { useState, useEffect } from "react"
import { toast } from "sonner"
import {
  ExternalLink, Check, X, Loader2, TestTube2,
  Eye, EyeOff, Star, Globe, Diamond, Leaf, Zap,
  Brain, Sparkles, Shell, Hexagon, Cloud, Smile, Flame, Waves,
  Search, Bot, BarChart3, Radio, Mail,
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
            <h3 className="text-sm font-medium">Pipeline Configuration</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Configure scraping, validation, and scoring behavior.
            </p>
          </div>
          <Separator />
          <Card>
            <CardContent className="pt-6">
              <Badge variant="secondary">Coming soon</Badge>
              <p className="text-sm text-muted-foreground mt-2">
                ICP configuration, scraper settings, validation rules, and scoring weights.
              </p>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="email" className="mt-4 space-y-4">
          <SMTPConfigTab />
        </TabsContent>

        <TabsContent value="integrations" className="mt-4 space-y-4">
          <div>
            <h3 className="text-sm font-medium">Integrations</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Connect CRM and messaging platforms.
            </p>
          </div>
          <Separator />
          <Card>
            <CardHeader className="pb-3">
              <div className="flex items-center justify-between">
                <CardTitle className="text-sm font-medium flex items-center gap-2">
                  <Globe className="size-4" />
                  HubSpot CRM
                </CardTitle>
                <Badge variant="secondary" className="text-xs">Coming soon</Badge>
              </div>
              <CardDescription className="text-xs">
                Push leads to HubSpot as contacts. Bidirectional sync with field mapping.
              </CardDescription>
            </CardHeader>
          </Card>
        </TabsContent>
      </Tabs>
    </div>
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

function SMTPConfigTab() {
  const [host, setHost] = useState("")
  const [port, setPort] = useState("587")
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [fromName, setFromName] = useState("Yupcha")
  const [maxPerHour, setMaxPerHour] = useState("50")
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testEmail, setTestEmail] = useState("")
  const [configured, setConfigured] = useState(false)

  useEffect(() => {
    fetch("/api/outreach/smtp/status")
      .then(r => r.json())
      .then(data => {
        setConfigured(data.configured)
        if (data.host) setHost(data.host)
        if (data.email) setEmail(data.email)
        if (data.from_name) setFromName(data.from_name)
        if (data.max_per_hour) setMaxPerHour(String(data.max_per_hour))
      })
      .catch(() => {})
  }, [])

  const handleSave = async () => {
    setSaving(true)
    try {
      await fetch("/api/outreach/smtp/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          smtp_host: host,
          smtp_port: parseInt(port),
          smtp_email: email,
          ...(password && { smtp_password: password }),
          smtp_from_name: fromName,
          smtp_max_per_hour: parseInt(maxPerHour),
        }),
      })
      toast.success("SMTP configuration saved")
      setPassword("")
      setConfigured(true)
    } catch {
      toast.error("Failed to save SMTP config")
    }
    setSaving(false)
  }

  const handleTest = async () => {
    if (!testEmail) { toast.error("Enter test email address"); return }
    setTesting(true)
    try {
      const res = await fetch("/api/outreach/smtp/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ to_email: testEmail }),
      })
      if (res.ok) {
        toast.success(`Test email sent to ${testEmail}`)
      } else {
        const data = await res.json()
        toast.error(data.detail || "Test failed")
      }
    } catch {
      toast.error("Test failed")
    }
    setTesting(false)
  }

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
                placeholder="Yupcha"
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
            <Button size="sm" onClick={handleSave} disabled={saving || !host || !email}>
              {saving ? <Loader2 className="size-3 animate-spin mr-1" /> : null}
              Save
            </Button>
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
              <Button size="sm" variant="outline" onClick={handleTest} disabled={testing || !configured}>
                {testing ? <Loader2 className="size-3 animate-spin mr-1" /> : <Mail className="size-3 mr-1" />}
                Send Test
              </Button>
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
