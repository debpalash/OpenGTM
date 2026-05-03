import { Clock, MessageSquare, Zap, Users } from "lucide-react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"

export default function CampaignsPage() {
  return (
    <div className="p-6 max-w-4xl mx-auto space-y-6">
      <div>
        <h2 className="text-lg font-semibold">Campaigns</h2>
        <p className="text-sm text-muted-foreground">
          Build AI-powered outreach sequences. Auto-personalize messages and schedule follow-ups.
        </p>
      </div>

      <Separator />

      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <MessageSquare className="size-4" />
              Sequence Builder
            </CardTitle>
            <CardDescription>
              Create multi-step email sequences with AI-generated personalization.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Badge variant="secondary">Coming soon</Badge>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Clock className="size-4" />
              Timing Rules
            </CardTitle>
            <CardDescription>
              Set send windows, follow-up delays, and timezone-aware scheduling.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Badge variant="secondary">Coming soon</Badge>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Zap className="size-4" />
              AI Message Generator
            </CardTitle>
            <CardDescription>
              Generate personalized outreach using lead data and your value proposition.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Badge variant="secondary">Coming soon</Badge>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <Users className="size-4" />
              Engagement Tracking
            </CardTitle>
            <CardDescription>
              Track opens, replies, bounces, and engagement across all channels.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Badge variant="secondary">Coming soon</Badge>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
