import { Clock, MessageSquare, Zap, Users } from "lucide-react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Separator } from "@/components/ui/separator"

export default function OutreachPage() {
  return (
    <div className="p-6 max-w-4xl mx-auto space-y-6">
      {/* Header */}
      <div>
        <h2 className="text-lg font-semibold">Outreach</h2>
        <p className="text-sm text-muted-foreground">
          AI-powered lead messaging, follow-ups, and engagement tracking.
        </p>
      </div>

      <Separator />

      {/* Feature Cards */}
      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium flex items-center gap-2">
              <MessageSquare className="size-4" />
              Email Sequences
            </CardTitle>
            <CardDescription>
              Create AI-personalized email sequences for your leads.
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
              Follow-up Scheduler
            </CardTitle>
            <CardDescription>
              Automated follow-up reminders based on lead engagement.
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
              Generate personalized outreach messages using your ICP and lead data.
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
              Track opens, replies, and engagement across all outreach channels.
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
