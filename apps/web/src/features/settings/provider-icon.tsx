import {
  Globe,
  Diamond,
  Leaf,
  Zap,
  Brain,
  Sparkles,
  Shell,
  Hexagon,
  Cloud,
  Smile,
  Flame,
  Waves,
  Search,
  Bot,
  Radio,
  Mail
} from "lucide-react"

// The API names provider icons with emoji; map them to Lucide components.
// Emoji are never rendered: unknown names fall back to a generic Bot icon.
const PROVIDER_ICON_MAP: Record<
  string,
  React.ComponentType<{ className?: string }>
> = {
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
  "🌍": Globe
}

export function ProviderIcon({ icon }: { icon: string }) {
  const Icon = PROVIDER_ICON_MAP[icon] || Bot
  return <Icon className="size-5 text-muted-foreground" />
}
