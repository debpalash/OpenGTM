import { useEffect, useState } from "react"
import { useLocation, useNavigate } from "react-router-dom"
import { Building2, Radar, Search, Users } from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  COLLECTION_CLARIFICATION_EVENT,
  CHAT_DRAFT_EVENT,
  type CollectionClarification,
  type CollectionIntent,
} from "@/lib/api"

const ICONS: Record<CollectionIntent, typeof Building2> = {
  company_research: Building2,
  people_at_company: Users,
  technology_users: Search,
  signal_monitor: Radar,
  market_search: Search,
}

export function CollectionIntentDialog() {
  const navigate = useNavigate()
  const location = useLocation()
  const [decision, setDecision] = useState<CollectionClarification | null>(null)

  useEffect(() => {
    const onClarification = (event: Event) => {
      const detail = (event as CustomEvent<CollectionClarification>).detail
      if (detail?.clarification_required) setDecision(detail)
    }
    window.addEventListener(COLLECTION_CLARIFICATION_EVENT, onClarification)
    return () => window.removeEventListener(COLLECTION_CLARIFICATION_EVENT, onClarification)
  }, [])

  const choose = (route: string, draft: string) => {
    setDecision(null)
    if (route === "/chat" && location.pathname === "/chat") {
      window.dispatchEvent(new CustomEvent(CHAT_DRAFT_EVENT, { detail: { draft } }))
      return
    }
    const params = new URLSearchParams()
    if (route === "/chat") params.set("draft", draft)
    if (route === "/watches") {
      params.set("create", "1")
      params.set("target", draft)
    }
    navigate(`${route}?${params.toString()}`)
  }

  return (
    <Dialog open={Boolean(decision)} onOpenChange={(open) => !open && setDecision(null)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            What do you want to do with {decision?.entity || decision?.domain || "this request"}?
          </DialogTitle>
          <DialogDescription>
            {decision?.clarification_kind === "company_team"
              ? "Choose the result you need. Each workflow uses different sources and acceptance rules; no collection task has been queued."
              : "A domain is a company target, not a market search. Choose the GTM workflow you need; no collection task has been queued yet."}
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-2">
          {decision?.options.map((option) => {
            const Icon = ICONS[option.intent]
            return (
              <Button
                key={option.key}
                variant="outline"
                className="h-auto justify-start gap-3 p-3 text-left"
                onClick={() => choose(option.route, option.draft)}
              >
                <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                  <Icon className="size-4" />
                </span>
                <span className="min-w-0">
                  <span className="block text-sm font-medium">{option.label}</span>
                  <span className="mt-0.5 block whitespace-normal text-xs font-normal text-muted-foreground">
                    {option.description}
                  </span>
                </span>
              </Button>
            )
          })}
        </div>
      </DialogContent>
    </Dialog>
  )
}
