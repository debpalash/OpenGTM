import { useState, useEffect } from "react"
import { toast } from "sonner"
import {
  Activity, Users, TrendingUp, Code, Globe, Newspaper,
  BarChart3, MessageSquare, RefreshCw, Loader2, Filter,
  Bell, Check, ExternalLink,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Separator } from "@/components/ui/separator"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"

const SIGNAL_ICONS: Record<string, React.ComponentType<{ className?: string }>> = {
  hiring: Users,
  funding: TrendingUp,
  tech_change: Code,
  website_change: Globe,
  news: Newspaper,
  growth: BarChart3,
  social_activity: MessageSquare,
}

const SIGNAL_COLORS: Record<string, string> = {
  hiring: "text-blue-400 bg-blue-400/10",
  funding: "text-emerald-400 bg-emerald-400/10",
  tech_change: "text-purple-400 bg-purple-400/10",
  website_change: "text-amber-400 bg-amber-400/10",
  news: "text-rose-400 bg-rose-400/10",
  growth: "text-sky-400 bg-sky-400/10",
  social_activity: "text-indigo-400 bg-indigo-400/10",
}

interface Signal {
  id: string
  lead_id: number
  company: string
  signal_type: string
  title: string
  description: string
  source: string
  source_url: string
  weight: number
  created_at: number
  read: boolean
}

export default function SignalsPage() {
  const [signals, setSignals] = useState<Signal[]>([])
  const [counts, setCounts] = useState<Record<string, number>>({})
  const [loading, setLoading] = useState(true)
  const [scanning, setScanning] = useState(false)
  const [filter, setFilter] = useState("all")

  const fetchSignals = async () => {
    try {
      const params = filter !== "all" ? `?signal_type=${filter}` : ""
      const res = await fetch(`/api/signals${params}`)
      const data = await res.json()
      setSignals(data.signals || [])
      setCounts(data.counts || {})
    } catch { /* ignore */ }
    setLoading(false)
  }

  useEffect(() => { fetchSignals() }, [filter])

  const runScan = async () => {
    setScanning(true)
    try {
      const res = await fetch("/api/signals/scan", { method: "POST" })
      const data = await res.json()
      if (data.status === "started") {
        toast.success("Signal scan started — checking job boards for new signals…")
        setTimeout(fetchSignals, 8000)
      } else {
        toast.success(`Scan complete: ${data.signals_found ?? 0} signals found`)
        fetchSignals()
      }
    } catch {
      toast.error("Scan failed")
    }
    setScanning(false)
  }

  const markAllRead = async () => {
    const unread = signals.filter(s => !s.read).map(s => s.id)
    if (unread.length === 0) return
    await fetch("/api/signals/mark-read", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ signal_ids: unread }),
    })
    toast.success("All marked as read")
    fetchSignals()
  }

  const formatTime = (ts: number) => {
    const d = new Date(ts * 1000)
    const now = Date.now()
    const diff = now - d.getTime()
    if (diff < 3600000) return `${Math.floor(diff / 60000)}m ago`
    if (diff < 86400000) return `${Math.floor(diff / 3600000)}h ago`
    return d.toLocaleDateString()
  }

  return (
    <div className="flex flex-col gap-4 p-4">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-base font-semibold flex items-center gap-2">
            <Activity className="size-4" />
            Signal Monitor
          </h2>
          <p className="text-xs text-muted-foreground mt-0.5">
            Buying signals detected from your leads — hiring, funding, growth, and more.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={markAllRead} className="h-7 text-xs gap-1">
            <Check className="size-3" /> Mark all read
          </Button>
          <Button size="sm" onClick={runScan} disabled={scanning} className="h-7 text-xs gap-1">
            {scanning ? <Loader2 className="size-3 animate-spin" /> : <RefreshCw className="size-3" />}
            Scan Now
          </Button>
        </div>
      </div>

      <Separator />

      {/* Stats */}
      <div className="flex items-center gap-4 text-xs">
        <span className="flex items-center gap-1.5 text-muted-foreground">
          <Bell className="size-3" />
          <span className="font-medium text-foreground">{counts.unread || 0}</span> unread
        </span>
        <span className="text-muted-foreground">
          {counts.total || 0} total signals
        </span>
        <div className="ml-auto">
          <Select value={filter} onValueChange={(v) => setFilter(v ?? "all")}>
            <SelectTrigger className="w-[140px] h-7 text-xs">
              <Filter className="size-3 mr-1" />
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All types</SelectItem>
              <SelectItem value="hiring">Hiring</SelectItem>
              <SelectItem value="funding">Funding</SelectItem>
              <SelectItem value="tech_change">Tech Change</SelectItem>
              <SelectItem value="growth">Growth</SelectItem>
              <SelectItem value="news">News</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      {/* Signal Feed */}
      {loading ? (
        <div className="text-sm text-muted-foreground py-8 text-center">Loading signals...</div>
      ) : signals.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-16 text-center">
          <Activity className="size-10 text-muted-foreground/20 mb-4" />
          <p className="text-sm text-muted-foreground mb-1">No signals detected yet</p>
          <p className="text-xs text-muted-foreground/60 mb-4 max-w-xs">
            Run a scan to detect hiring activity, funding rounds, and growth signals from your hot leads.
          </p>
          <Button size="sm" onClick={runScan} disabled={scanning} className="gap-1">
            {scanning ? <Loader2 className="size-3 animate-spin" /> : <RefreshCw className="size-3" />}
            Run First Scan
          </Button>
        </div>
      ) : (
        <div className="space-y-1.5">
          {signals.map(signal => {
            const Icon = SIGNAL_ICONS[signal.signal_type] || Activity
            const colorClass = SIGNAL_COLORS[signal.signal_type] || "text-muted-foreground bg-muted"
            return (
              <div
                key={signal.id}
                className={`flex items-start gap-3 px-3 py-2.5 rounded-lg border transition-colors ${
                  signal.read ? "opacity-60" : "border-border/50"
                }`}
              >
                <div className={`size-7 rounded-md flex items-center justify-center shrink-0 mt-0.5 ${colorClass}`}>
                  <Icon className="size-3.5" />
                </div>
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-medium">{signal.title}</span>
                    {!signal.read && (
                      <span className="size-1.5 rounded-full bg-blue-400 shrink-0" />
                    )}
                  </div>
                  {signal.description && (
                    <p className="text-[11px] text-muted-foreground mt-0.5 line-clamp-2">
                      {signal.description}
                    </p>
                  )}
                  <div className="flex items-center gap-3 mt-1 text-[10px] text-muted-foreground/60">
                    <span>{signal.company}</span>
                    <span>{signal.source}</span>
                    {signal.source_url && (
                      <a
                        href={signal.source_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="flex items-center gap-0.5 hover:underline"
                        onClick={(e) => e.stopPropagation()}
                      >
                        <ExternalLink className="size-2.5" /> Source
                      </a>
                    )}
                    <span className="ml-auto">{formatTime(signal.created_at)}</span>
                  </div>
                </div>
                <div className="flex items-center gap-1 shrink-0">
                  {Array.from({ length: Math.min(5, Math.ceil(signal.weight / 2)) }).map((_, i) => (
                    <span
                      key={i}
                      className={`size-1 rounded-full ${
                        i < Math.ceil(signal.weight / 2) ? "bg-amber-400" : "bg-muted"
                      }`}
                    />
                  ))}
                </div>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
