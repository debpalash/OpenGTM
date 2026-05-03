import { useState } from "react"
import { toast } from "sonner"
import {
  ExternalLink, Check, X, Loader2, TestTube2,
  Eye, EyeOff, Star,
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
            <span className="text-lg">{provider.icon}</span>
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
    <div className="p-6 max-w-4xl mx-auto">
      <Tabs defaultValue="providers">
        <TabsList>
          <TabsTrigger value="providers">AI Providers</TabsTrigger>
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

        <TabsContent value="integrations" className="mt-4 space-y-4">
          <div>
            <h3 className="text-sm font-medium">Integrations</h3>
            <p className="text-xs text-muted-foreground mt-1">
              Connect email, CRM, and messaging platforms.
            </p>
          </div>
          <Separator />
          <Card>
            <CardContent className="pt-6">
              <Badge variant="secondary">Coming soon</Badge>
              <p className="text-sm text-muted-foreground mt-2">
                SMTP, SendGrid, HubSpot, Salesforce, Slack integrations.
              </p>
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>
    </div>
  )
}
