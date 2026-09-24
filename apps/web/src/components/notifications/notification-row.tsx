import { ArrowUpRight, FileSpreadsheet, Radar, Workflow } from "lucide-react"
import { Link } from "react-router-dom"
import type { AppNotification } from "@/lib/notifications"
import "./notifications.css"

type NotificationRowProps = {
  notification: AppNotification
  compact?: boolean
  onNavigate?: () => void
}

const sources = {
  task: { label: "Task", icon: Workflow },
  workbook: { label: "Workbook", icon: FileSpreadsheet },
  signal: { label: "Signal", icon: Radar },
} as const

function notificationTime(createdAt: number) {
  const date = new Date(createdAt)
  if (!Number.isFinite(date.getTime())) return { label: "Recent", full: undefined }
  const age = Math.max(0, Date.now() - createdAt)
  const label = age < 60_000 ? "Just now"
    : age < 3_600_000 ? `${Math.floor(age / 60_000)}m ago`
    : age < 86_400_000 ? `${Math.floor(age / 3_600_000)}h ago`
    : date.toLocaleDateString(undefined, { month: "short", day: "numeric", year: date.getFullYear() === new Date().getFullYear() ? undefined : "numeric" })
  return { label, full: date.toLocaleString() }
}

export function NotificationRow({ notification, compact = false, onNavigate }: NotificationRowProps) {
  const source = sources[notification.category]
  const Icon = source.icon
  const time = notificationTime(notification.createdAt)

  return <Link
    to={notification.href}
    onClick={onNavigate}
    className={`gtm-notification-row${compact ? " gtm-notification-row--compact" : ""}`}
    data-severity={notification.severity}
    aria-label={`Open ${source.label.toLowerCase()}: ${notification.title} (${notification.severity})`}
  >
    <span className="gtm-notification-row__icon" aria-hidden="true"><Icon size={18} /></span>
    <span className="gtm-notification-row__body">
      <span className="gtm-notification-row__meta">
        <span className="gtm-notification-row__severity">{notification.severity}</span>
        <span className="gtm-notification-row__source">{source.label}</span>
        {time.full ? <time dateTime={new Date(notification.createdAt).toISOString()} title={time.full} className="gtm-notification-row__time">{time.label}</time>
          : <span className="gtm-notification-row__time">{time.label}</span>}
      </span>
      <span className="gtm-notification-row__title">{notification.title}</span>
      {notification.description && <span className="gtm-notification-row__description">{notification.description}</span>}
    </span>
    <ArrowUpRight size={17} className="gtm-notification-row__arrow" aria-hidden="true" />
  </Link>
}
