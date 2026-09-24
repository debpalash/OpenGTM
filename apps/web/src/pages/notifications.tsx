import { useState } from "react"
import { Bell, Check, CircleAlert, RefreshCw, TriangleAlert } from "lucide-react"
import { NotificationRow } from "@/components/notifications/notification-row"
import { useNotifications } from "@/lib/notifications"
import type { AppNotification } from "@/lib/notifications"
import "@/components/notifications/notifications.css"

type Filter = "all" | AppNotification["severity"]

export default function NotificationsPage() {
  const query = useNotifications()
  const [filter, setFilter] = useState<Filter>("all")
  const notifications = query.data ?? []
  const critical = notifications.filter(item => item.severity === "critical").length
  const urgent = notifications.filter(item => item.severity === "urgent").length
  const visible = notifications
    .filter(item => filter === "all" || item.severity === filter)
    .sort((a, b) => b.createdAt - a.createdAt || a.id.localeCompare(b.id))

  return <div className="gtm-attention">
    <header className="gtm-attention__header">
      <div className="gtm-attention__eyebrow"><span className="gtm-attention__pulse" aria-hidden="true" /> WORKSPACE · ATTENTION</div>
      <h1>What needs your attention.</h1>
      <p>Recent failed work and high-intent signals, together in one place.</p>
    </header>

    <section className="gtm-attention__overview" aria-label="Recent attention summary">
      <div className="gtm-attention__intro">
        <Bell size={19} aria-hidden="true" />
        <div><strong>Stay on top of the important things.</strong><span>Showing recent items from this workspace, not an all-time unread total.</span></div>
      </div>
      <div className="gtm-attention__totals">
        <div className="gtm-attention__total" data-severity="critical">
          <TriangleAlert size={19} aria-hidden="true" />
          <span className="gtm-attention__number">{query.isPending || query.isError ? "—" : critical}</span>
          <span className="gtm-attention__total-label">Critical <small>Failed tasks &amp; workbooks</small></span>
        </div>
        <div className="gtm-attention__total" data-severity="urgent">
          <CircleAlert size={19} aria-hidden="true" />
          <span className="gtm-attention__number">{query.isPending || query.isError ? "—" : urgent}</span>
          <span className="gtm-attention__total-label">Urgent <small>High-intent signals</small></span>
        </div>
      </div>
    </section>

    <section className="gtm-attention__feed" aria-label="Recent attention items">
      <div className="gtm-attention__feed-heading">
        <div><span className="gtm-attention__section-kicker">THE LATEST</span><h2>Recent items</h2></div>
        <button type="button" className="gtm-attention__refresh" onClick={() => void query.refetch()} disabled={query.isFetching}>
          <RefreshCw size={15} className={query.isFetching ? "gtm-attention__spinning" : ""} aria-hidden="true" />
          {query.isFetching ? "Refreshing…" : "Refresh"}
        </button>
      </div>
      <div className="gtm-attention__toolbar">
        <div className="gtm-attention__filters" role="group" aria-label="Filter by severity">
          {(["all", "critical", "urgent"] as const).map(option => <button key={option} type="button" aria-pressed={filter === option}
            onClick={() => setFilter(option)} className="gtm-attention__filter">
            {option === "all" ? "All" : option === "critical" ? "Critical" : "Urgent"}
            {!query.isPending && !query.isError && <span className="gtm-attention__filter-count">{option === "all" ? notifications.length : option === "critical" ? critical : urgent}</span>}
          </button>)}
        </div>
        {!query.isPending && !query.isError && <span className="gtm-attention__shown" role="status">{visible.length} recent {visible.length === 1 ? "item" : "items"}</span>}
      </div>
      {query.isPending ? <div className="gtm-attention__state" role="status">
        <span className="gtm-attention__state-icon"><Bell size={21} aria-hidden="true" /></span><h3>Looking for recent items…</h3><p>Checking tasks, workbooks and signals.</p>
      </div> : query.isError ? <div className="gtm-attention__state" role="alert">
        <span className="gtm-attention__state-icon"><CircleAlert size={21} aria-hidden="true" /></span><h3>We couldn’t load your recent items.</h3><p>Check your connection and try again.</p>
        <button type="button" className="gtm-attention__state-action" onClick={() => void query.refetch()} disabled={query.isFetching}>Try again</button>
      </div> : visible.length === 0 ? <div className="gtm-attention__state">
        <span className="gtm-attention__state-icon"><Check size={21} aria-hidden="true" /></span>
        <h3>{notifications.length === 0 ? "All clear for now." : `No ${filter} items right now.`}</h3>
        <p>{notifications.length === 0 ? "No failed work or unread high-intent signals in your recent activity." : "Try another filter to see the rest of your recent items."}</p>
        {notifications.length > 0 && <button type="button" className="gtm-attention__state-action" onClick={() => setFilter("all")}>View all items</button>}
      </div> : <div className="gtm-attention__list">{visible.map(notification => <NotificationRow key={notification.id} notification={notification} />)}</div>}
      <p className="gtm-attention__footnote">This view reflects recent activity only. Open an item to see its latest details.</p>
    </section>
  </div>
}
