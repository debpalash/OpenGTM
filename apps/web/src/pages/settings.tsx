import { useState, useEffect } from "react"
import { Settings2, Check, Loader2, ExternalLink, Eye, EyeOff, Zap, Star, ChevronDown, BrainCircuit, Plug, Webhook } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { toast } from "sonner"

const API = ""

interface Provider {
  id: string; name: string; icon: string; configured: boolean
  api_key_masked: string; base_url: string; model: string
  default_model: string; docs_url: string; free_tier: string
  is_default: boolean; openai_compatible: boolean
}
interface ModelInfo { id: string; name: string; context: number; free: boolean }

// ── Sidebar sections
const SECTIONS = [
  { id: "models", label: "Models", icon: BrainCircuit },
  { id: "integrations", label: "Integrations", icon: Plug },
  { id: "webhooks", label: "Webhooks", icon: Webhook },
] as const
type Section = typeof SECTIONS[number]["id"]

// ── Top tabs per section
const TABS: Record<Section, string[]> = {
  models: ["LLM Providers", "Model Browser"],
  integrations: ["Search APIs", "Data Sources", "Enrichment"],
  webhooks: ["Outgoing", "Incoming"],
}

export default function SettingsPage() {
  const [section, setSection] = useState<Section>("models")
  const [tab, setTab] = useState(0)
  const [providers, setProviders] = useState<Provider[]>([])
  const [loading, setLoading] = useState(true)
  const [editingId, setEditingId] = useState<string | null>(null)
  const [editKey, setEditKey] = useState("")
  const [editModel, setEditModel] = useState("")
  const [testingId, setTestingId] = useState<string | null>(null)
  const [testResults, setTestResults] = useState<Record<string, { status: string; response?: string; error?: string }>>({})
  const [showKeys, setShowKeys] = useState<Record<string, boolean>>({})
  const [modelPicker, setModelPicker] = useState<string | null>(null)
  const [models, setModels] = useState<Record<string, ModelInfo[]>>({})
  const [modelsLoading, setModelsLoading] = useState<string | null>(null)

  useEffect(() => { fetchProviders() }, [])
  useEffect(() => { setTab(0) }, [section])

  const fetchProviders = () => {
    setLoading(true)
    fetch(`${API}/api/settings/providers`).then(r => r.json())
      .then(d => setProviders(d.providers || []))
      .catch(() => toast.error("Failed to load"))
      .finally(() => setLoading(false))
  }

  const fetchModels = async (pid: string) => {
    if (models[pid]) { setModelPicker(modelPicker === pid ? null : pid); return }
    setModelsLoading(pid)
    try {
      const q = pid === "openrouter" ? "?free_only=true" : ""
      const d = await (await fetch(`${API}/api/settings/models/${pid}${q}`)).json()
      setModels(p => ({ ...p, [pid]: d.models || [] }))
      setModelPicker(pid)
    } catch { toast.error("Failed to fetch models") }
    finally { setModelsLoading(null) }
  }

  const selectModel = (pid: string, mid: string) => {
    setEditModel(mid); setModelPicker(null)
    if (editingId !== pid) {
      fetch(`${API}/api/settings/providers/${pid}`, {
        method: "PUT", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: mid }),
      }).then(() => { toast.success(`Model → ${mid}`); fetchProviders() })
    }
  }

  const saveProvider = async (id: string) => {
    const body: Record<string, string | boolean> = {}
    if (editKey) body.api_key = editKey
    if (editModel) body.model = editModel
    const res = await fetch(`${API}/api/settings/providers/${id}`, {
      method: "PUT", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
    if (res.ok) { toast.success(`${id} updated`); setEditingId(null); setEditKey(""); setEditModel(""); fetchProviders() }
  }

  const setDefault = async (id: string) => {
    await fetch(`${API}/api/settings/providers/${id}`, {
      method: "PUT", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ set_default: true }),
    })
    fetchProviders(); toast.success(`Default → ${id}`)
  }

  const testProvider = async (id: string) => {
    setTestingId(id)
    try {
      const d = await (await fetch(`${API}/api/settings/providers/${id}/test`, { method: "POST" })).json()
      setTestResults(p => ({ ...p, [id]: d }))
      d.status === "ok" ? toast.success(`✓ ${id}: "${d.response}"`) : toast.error(`✗ ${id}: ${d.error}`)
    } catch { setTestResults(p => ({ ...p, [id]: { status: "error", error: "Network error" } })) }
    finally { setTestingId(null) }
  }

  const fmtCtx = (c: number) => c >= 1e6 ? `${(c/1e6).toFixed(1)}M` : c >= 1e3 ? `${(c/1e3).toFixed(0)}K` : `${c}`

  const tabs = TABS[section]

  return (
    <div className="flex h-full overflow-hidden">
      {/* ── Settings Sidebar ── */}
      <div className="w-44 border-r border-border flex flex-col shrink-0 bg-card/30">
        <div className="p-3 border-b border-border">
          <h2 className="text-xs font-semibold flex items-center gap-1.5">
            <Settings2 size={13} className="text-primary" /> Settings
          </h2>
        </div>
        <nav className="flex flex-col gap-0.5 p-2">
          {SECTIONS.map(s => (
            <button
              key={s.id}
              onClick={() => setSection(s.id)}
              className={`flex items-center gap-2 px-2.5 py-1.5 rounded-md text-[11px] font-medium transition-all ${
                section === s.id
                  ? "bg-primary/10 text-primary"
                  : "text-muted-foreground hover:text-foreground hover:bg-card"
              }`}
            >
              <s.icon size={13} />
              {s.label}
            </button>
          ))}
        </nav>
      </div>

      {/* ── Content Area ── */}
      <div className="flex-1 flex flex-col min-w-0 min-h-0">
        {/* Top Tabs */}
        <div className="border-b border-border px-4 flex gap-0 shrink-0">
          {tabs.map((t, i) => (
            <button
              key={t}
              onClick={() => setTab(i)}
              className={`px-3 py-2.5 text-[11px] font-medium border-b-2 transition-all ${
                tab === i
                  ? "border-primary text-primary"
                  : "border-transparent text-muted-foreground hover:text-foreground"
              }`}
            >
              {t}
            </button>
          ))}
        </div>

        {/* Tab Content — scrollable within remaining height */}
        <div className="flex-1 overflow-y-auto min-h-0">
          <div className="p-4">
            {section === "models" && tab === 0 && <ProvidersPanel
              providers={providers} loading={loading} editingId={editingId} editKey={editKey}
              editModel={editModel} testingId={testingId} testResults={testResults} showKeys={showKeys}
              modelPicker={modelPicker} models={models} modelsLoading={modelsLoading} fmtCtx={fmtCtx}
              setEditingId={setEditingId} setEditKey={setEditKey} setEditModel={setEditModel}
              setShowKeys={setShowKeys} fetchModels={fetchModels} selectModel={selectModel}
              saveProvider={saveProvider} setDefault={setDefault} testProvider={testProvider}
            />}
            {section === "models" && tab === 1 && <ModelBrowser />}
            {section === "integrations" && <IntegrationsPanel tab={tab} />}
            {section === "webhooks" && <WebhooksPanel tab={tab} />}
          </div>
        </div>
      </div>
    </div>
  )
}

// ── LLM Providers Tab ──
function ProvidersPanel({ providers, loading, editingId, editKey, editModel, testingId, testResults,
  showKeys, modelPicker, models, modelsLoading, fmtCtx, setEditingId, setEditKey, setEditModel,
  setShowKeys, fetchModels, selectModel, saveProvider, setDefault, testProvider }: any) {

  if (loading) return (
    <div className="text-center py-20">
      <Loader2 className="mx-auto mb-3 animate-spin text-primary" size={28} />
      <p className="text-xs text-muted-foreground">Loading providers…</p>
    </div>
  )

  return (
    <div className="space-y-2.5">
      {providers.map((p: Provider) => {
        const isEdit = editingId === p.id
        const isTesting = testingId === p.id
        const result = testResults[p.id]
        const keyVis = showKeys[p.id]
        const pickerOpen = modelPicker === p.id
        const mList = models[p.id] || []
        const mLoading = modelsLoading === p.id

        return (
          <Card key={p.id} className={`p-3 transition-all ${p.is_default ? "ring-1 ring-primary/30 bg-primary/[0.02]" : ""}`}>
            <div className="flex items-start justify-between gap-3">
              <div className="flex-1 min-w-0">
                {/* Header */}
                <div className="flex items-center gap-1.5 mb-1 flex-wrap">
                  <span className="text-sm">{p.icon}</span>
                  <span className="text-[11px] font-semibold">{p.name}</span>
                  {p.configured && <Badge variant="secondary" className="text-[8px] h-3.5 bg-emerald-500/10 text-emerald-400 border-emerald-500/20"><Check size={7} className="mr-0.5"/>Active</Badge>}
                  {!p.configured && <Badge variant="secondary" className="text-[8px] h-3.5 bg-amber-500/10 text-amber-400 border-amber-500/20">Not Set</Badge>}
                  {p.is_default && <Badge variant="secondary" className="text-[8px] h-3.5 bg-primary/10 text-primary border-primary/20"><Star size={7} className="mr-0.5"/>Default</Badge>}
                </div>

                <p className="text-[9px] text-muted-foreground mb-1.5"><Zap size={8} className="inline mr-0.5 text-amber-400"/>Free: {p.free_tier}</p>

                {/* Model */}
                <div className="flex items-center gap-1.5 mb-1.5">
                  <span className="text-[9px] text-muted-foreground/60">Model:</span>
                  <button onClick={() => fetchModels(p.id)}
                    className="inline-flex items-center gap-1 text-[9px] font-mono bg-card hover:bg-accent px-1.5 py-0.5 rounded border border-border transition-colors max-w-[240px]">
                    <span className="truncate">{isEdit && editModel ? editModel : p.model}</span>
                    {mLoading ? <Loader2 size={8} className="animate-spin shrink-0"/> : <ChevronDown size={8} className={`shrink-0 transition-transform ${pickerOpen?"rotate-180":""}`}/>}
                  </button>
                </div>

                {/* Model picker */}
                {pickerOpen && mList.length > 0 && (
                  <div className="mb-1.5 rounded border border-border bg-card overflow-hidden">
                    <ScrollArea className="max-h-[180px]">
                      {mList.map((m: ModelInfo) => (
                        <button key={m.id} onClick={() => selectModel(p.id, m.id)}
                          className={`w-full text-left px-2.5 py-1 hover:bg-accent transition-colors border-b border-border/40 last:border-0 ${(isEdit?editModel:p.model)===m.id?"bg-primary/5":""}`}>
                          <div className="flex items-center justify-between gap-2">
                            <div className="min-w-0">
                              <span className="text-[9px] font-medium block truncate">{m.name}</span>
                              <span className="text-[8px] font-mono text-muted-foreground block truncate">{m.id}</span>
                            </div>
                            <div className="flex items-center gap-1 shrink-0">
                              {m.context > 0 && <span className="text-[8px] text-muted-foreground">{fmtCtx(m.context)}</span>}
                              {m.free && <Badge variant="secondary" className="text-[7px] h-3 bg-emerald-500/10 text-emerald-400 px-1">FREE</Badge>}
                              {(isEdit?editModel:p.model)===m.id && <Check size={9} className="text-primary"/>}
                            </div>
                          </div>
                        </button>
                      ))}
                    </ScrollArea>
                  </div>
                )}

                {/* Key */}
                <div className="flex items-center gap-1 text-[9px] font-mono text-muted-foreground">
                  <span className="text-muted-foreground/50">Key:</span>
                  <span>{keyVis && p.api_key_masked ? p.api_key_masked : (p.configured ? "••••••••••••" : "not set")}</span>
                  {p.configured && <button onClick={() => setShowKeys((v: any) => ({...v, [p.id]: !v[p.id]}))} className="p-0.5 hover:text-foreground">
                    {keyVis ? <EyeOff size={9}/> : <Eye size={9}/>}
                  </button>}
                </div>

                {/* Edit form */}
                {isEdit && (
                  <div className="mt-2 space-y-1.5 p-2.5 rounded bg-card border border-border">
                    <div>
                      <label className="text-[9px] text-muted-foreground font-medium">API Key</label>
                      <Input value={editKey} onChange={e => setEditKey(e.target.value)} placeholder="sk-..." className="h-6 text-[10px] font-mono mt-0.5" type="password"/>
                    </div>
                    <div>
                      <label className="text-[9px] text-muted-foreground font-medium">Model</label>
                      <Input value={editModel} onChange={e => setEditModel(e.target.value)} placeholder={p.default_model} className="h-6 text-[10px] font-mono mt-0.5"/>
                    </div>
                    <div className="flex gap-1">
                      <Button size="sm" className="h-5 text-[9px] px-2" onClick={() => saveProvider(p.id)}><Check size={9} className="mr-0.5"/>Save</Button>
                      <Button size="sm" variant="ghost" className="h-5 text-[9px] px-2" onClick={() => {setEditingId(null);setEditKey("");setEditModel("")}}>Cancel</Button>
                    </div>
                  </div>
                )}

                {/* Test result */}
                {result && (
                  <div className={`mt-1.5 text-[9px] px-2 py-0.5 rounded ${result.status==="ok"?"bg-emerald-500/10 text-emerald-400":"bg-red-500/10 text-red-400"}`}>
                    {result.status==="ok" ? `✓ "${result.response}"` : `✗ ${result.error}`}
                  </div>
                )}
              </div>

              {/* Actions */}
              <div className="flex flex-col gap-0.5 shrink-0">
                {!isEdit && <Button variant="outline" size="sm" className="h-5 text-[9px] px-2"
                  onClick={() => {setEditingId(p.id);setEditKey("");setEditModel(p.model)}}>Configure</Button>}
                {p.configured && <Button variant="outline" size="sm" className="h-5 text-[9px] px-2" disabled={isTesting}
                  onClick={() => testProvider(p.id)}>
                  {isTesting ? <Loader2 size={9} className="animate-spin mr-0.5"/> : <Zap size={9} className="mr-0.5"/>}Test
                </Button>}
                {p.configured && !p.is_default && <Button variant="ghost" size="sm" className="h-5 text-[9px] px-2"
                  onClick={() => setDefault(p.id)}><Star size={9} className="mr-0.5"/>Default</Button>}
                <Button variant="ghost" size="sm" className="h-5 text-[9px] px-2"
                  onClick={() => window.open(p.docs_url,"_blank")}><ExternalLink size={9} className="mr-0.5"/>Key</Button>
              </div>
            </div>
          </Card>
        )
      })}
    </div>
  )
}

// ── Model Browser Tab (placeholder) ──
function ModelBrowser() {
  const [mods, setMods] = useState<ModelInfo[]>([])
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    fetch("/api/settings/models/openrouter?free_only=true").then(r=>r.json())
      .then(d => setMods(d.models||[]))
      .finally(() => setLoading(false))
  }, [])

  if (loading) return <div className="text-center py-16"><Loader2 className="mx-auto animate-spin text-primary" size={24}/><p className="text-xs text-muted-foreground mt-2">Loading models…</p></div>

  return (
    <div className="space-y-1">
      <p className="text-[10px] text-muted-foreground mb-3">OpenRouter free models — click to set as default</p>
      {mods.map(m => (
        <div key={m.id} className="flex items-center justify-between px-3 py-1.5 rounded hover:bg-card border border-transparent hover:border-border transition-all cursor-pointer group"
          onClick={() => {
            fetch("/api/settings/providers/openrouter", {method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({model:m.id})})
              .then(()=>toast.success(`OpenRouter model → ${m.id}`))
          }}>
          <div className="min-w-0">
            <span className="text-[10px] font-medium">{m.name}</span>
            <span className="text-[9px] font-mono text-muted-foreground ml-2">{m.id}</span>
          </div>
          <div className="flex items-center gap-2">
            {m.context > 0 && <span className="text-[9px] text-muted-foreground">{m.context >= 1e6 ? `${(m.context/1e6).toFixed(1)}M` : `${(m.context/1e3).toFixed(0)}K`} ctx</span>}
            <Badge variant="secondary" className="text-[7px] h-3 bg-emerald-500/10 text-emerald-400 px-1">FREE</Badge>
          </div>
        </div>
      ))}
    </div>
  )
}

// ── Integrations Panel ──
function IntegrationsPanel({ tab }: { tab: number }) {
  const integrations = [
    { name: "DuckDuckGo Search", status: "active", desc: "Web search for lead discovery", key: "Built-in" },
    { name: "Google Custom Search", status: "optional", desc: "Enhanced search with API key", key: "GOOGLE_API_KEY" },
    { name: "Hunter.io", status: "optional", desc: "Email finder & verification", key: "HUNTER_API_KEY" },
  ]
  const dataSources = [
    { name: "Clutch.co", status: "active", desc: "Directory scraping" },
    { name: "GoodFirms", status: "active", desc: "Directory scraping" },
    { name: "LinkedIn", status: "optional", desc: "Requires li_at cookie", key: "LINKEDIN_LI_AT_COOKIE" },
    { name: "Scribd", status: "optional", desc: "Document downloads", key: "SCRIBD_COOKIES" },
  ]
  const enrichment = [
    { name: "Website Scraper", status: "active", desc: "Extracts emails/phones from company sites" },
    { name: "Search Enricher", status: "active", desc: "DDG-based contact discovery" },
    { name: "Social Finder", status: "active", desc: "LinkedIn/Twitter profile matching" },
  ]

  const items = tab === 0 ? integrations : tab === 1 ? dataSources : enrichment

  return (
    <div className="space-y-2">
      {items.map(item => (
        <Card key={item.name} className="p-3">
          <div className="flex items-center justify-between">
            <div>
              <div className="flex items-center gap-2">
                <span className="text-[11px] font-semibold">{item.name}</span>
                <Badge variant="secondary" className={`text-[8px] h-3.5 ${
                  item.status === "active" ? "bg-emerald-500/10 text-emerald-400" : "bg-amber-500/10 text-amber-400"
                }`}>{item.status === "active" ? "Active" : "Optional"}</Badge>
              </div>
              <p className="text-[9px] text-muted-foreground mt-0.5">{item.desc}</p>
            </div>
            {"key" in item && (item as any).key && (
              <code className="text-[9px] bg-card px-1.5 py-0.5 rounded border border-border text-muted-foreground">{(item as any).key}</code>
            )}
          </div>
        </Card>
      ))}
    </div>
  )
}

// ── Webhooks Panel ──
function WebhooksPanel({ tab }: { tab: number }) {
  return (
    <div className="text-center py-16">
      <Webhook size={32} className="mx-auto mb-3 text-muted-foreground/30" />
      <p className="text-xs text-muted-foreground">
        {tab === 0 ? "Outgoing webhooks — notify external services when leads are collected" : "Incoming webhooks — receive leads from external sources"}
      </p>
      <p className="text-[10px] text-muted-foreground/60 mt-1">Coming soon</p>
    </div>
  )
}
