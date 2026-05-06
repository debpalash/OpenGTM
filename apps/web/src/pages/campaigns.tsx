import React, { useState } from "react"
import { toast } from "sonner"
import {
  MessageSquare, Zap, Users, Send, Copy, RefreshCw,
  Mail, Sparkles, CheckCircle2,
} from "lucide-react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Separator } from "@/components/ui/separator"
import { Textarea } from "@/components/ui/textarea"
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select"

interface GeneratedEmail {
  subject: string
  body: string
  company: string
}

export default function CampaignsPage() {
  const [leads, setLeads] = useState<Array<{ id: number; company: string; email: string; score: number }>>([])
  const [selectedTier, setSelectedTier] = useState("hot")
  const [tone, setTone] = useState("professional")
  const [valueProp, setValueProp] = useState("")
  const [generating, setGenerating] = useState(false)
  const [emails, setEmails] = useState<GeneratedEmail[]>([])
  const [copied, setCopied] = useState<number | null>(null)

  // Load leads on mount
  React.useEffect(() => {
    fetch(`/api/leads?limit=100&tier=${selectedTier}`)
      .then(r => r.json())
      .then(data => {
        const withEmail = (data.leads || []).filter((l: Record<string, string>) => l.email && l.email.includes("@"))
        setLeads(withEmail.slice(0, 20))
      })
      .catch(() => {})
  }, [selectedTier])

  const generateEmails = async () => {
    if (!valueProp.trim()) {
      toast.error("Enter your value proposition first")
      return
    }
    if (leads.length === 0) {
      toast.error("No leads with emails in this tier")
      return
    }

    setGenerating(true)
    setEmails([])

    try {
      const res = await fetch("/api/campaigns/generate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          lead_ids: leads.slice(0, 5).map(l => l.id),
          value_proposition: valueProp,
          tone,
        }),
      })
      const data = await res.json()
      setEmails(data.emails || [])
      toast.success(`Generated ${data.emails?.length || 0} personalized emails`)
    } catch {
      toast.error("Failed to generate emails")
    } finally {
      setGenerating(false)
    }
  }

  const copyEmail = (idx: number, email: GeneratedEmail) => {
    navigator.clipboard.writeText(`Subject: ${email.subject}\n\n${email.body}`)
    setCopied(idx)
    toast.success("Copied to clipboard")
    setTimeout(() => setCopied(null), 2000)
  }

  return (
    <div className="p-6 space-y-6">
      <div>
        <h2 className="text-lg font-semibold flex items-center gap-2">
          <Sparkles className="size-5" />
          Campaigns
        </h2>
        <p className="text-sm text-muted-foreground">
          Generate AI-personalized outreach emails from your scored leads.
        </p>
      </div>

      <Separator />

      {/* Controls */}
      <div className="grid gap-4 md:grid-cols-3">
        <div className="space-y-2">
          <label className="text-sm font-medium">Target Tier</label>
          <Select value={selectedTier} onValueChange={setSelectedTier}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="hot">🔥 Hot Leads</SelectItem>
              <SelectItem value="warm">🟡 Warm Leads</SelectItem>
              <SelectItem value="cold">🔵 Cold Leads</SelectItem>
            </SelectContent>
          </Select>
          <p className="text-xs text-muted-foreground">
            {leads.length} leads with emails
          </p>
        </div>

        <div className="space-y-2">
          <label className="text-sm font-medium">Tone</label>
          <Select value={tone} onValueChange={setTone}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="professional">Professional</SelectItem>
              <SelectItem value="friendly">Friendly</SelectItem>
              <SelectItem value="direct">Direct & Bold</SelectItem>
              <SelectItem value="consultative">Consultative</SelectItem>
            </SelectContent>
          </Select>
        </div>

        <div className="space-y-2">
          <label className="text-sm font-medium">Your Value Prop</label>
          <Input
            placeholder="e.g. We automate candidate tracking..."
            value={valueProp}
            onChange={(e) => setValueProp(e.target.value)}
          />
        </div>
      </div>

      <Textarea
        placeholder="Optional: Add more context about your product, target persona, or specific pain points you solve..."
        className="min-h-[80px]"
        value={valueProp.includes("\n") ? valueProp : ""}
        onChange={(e) => setValueProp(e.target.value)}
      />

      <Button
        onClick={generateEmails}
        disabled={generating || !valueProp.trim()}
        className="w-full"
      >
        {generating ? (
          <><RefreshCw className="size-4 mr-2 animate-spin" /> Generating personalized emails...</>
        ) : (
          <><Zap className="size-4 mr-2" /> Generate Outreach for Top {Math.min(5, leads.length)} Leads</>
        )}
      </Button>

      <Separator />

      {/* Generated Emails */}
      {emails.length > 0 && (
        <div className="space-y-4">
          <h3 className="text-sm font-medium flex items-center gap-2">
            <Mail className="size-4" />
            Generated Emails ({emails.length})
          </h3>

          {emails.map((email, idx) => (
            <Card key={idx} className="relative">
              <CardHeader className="pb-2">
                <div className="flex items-center justify-between">
                  <CardTitle className="text-sm font-medium">
                    {email.company}
                  </CardTitle>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => copyEmail(idx, email)}
                  >
                    {copied === idx ? (
                      <CheckCircle2 className="size-4 text-green-500" />
                    ) : (
                      <Copy className="size-4" />
                    )}
                  </Button>
                </div>
                <CardDescription className="font-medium text-foreground">
                  Subject: {email.subject}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <pre className="text-sm whitespace-pre-wrap font-sans text-muted-foreground leading-relaxed">
                  {email.body}
                </pre>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {/* Feature Cards */}
      {emails.length === 0 && (
        <div className="grid gap-4 md:grid-cols-2">
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <MessageSquare className="size-4" />
                AI Personalization
              </CardTitle>
              <CardDescription>
                Each email is uniquely crafted using the lead's company data, industry, and size.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Badge variant="secondary">Ready</Badge>
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Send className="size-4" />
                One-Click Copy
              </CardTitle>
              <CardDescription>
                Copy personalized emails to clipboard. Paste into Gmail, Outlook, or your CRM.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Badge variant="secondary">Ready</Badge>
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Users className="size-4" />
                Batch Generation
              </CardTitle>
              <CardDescription>
                Generate emails for your top-scored leads in bulk with one click.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Badge variant="secondary">Ready</Badge>
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium flex items-center gap-2">
                <Zap className="size-4" />
                Smart Scoring
              </CardTitle>
              <CardDescription>
                Hot leads get priority. Filter by tier to focus your outreach efforts.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Badge variant="secondary">Ready</Badge>
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  )
}
