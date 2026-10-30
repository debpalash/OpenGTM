import { useState } from "react"
import type { LucideIcon } from "lucide-react"
import assets from "@/lib/source-brand-assets.json"
import "./source-logo.css"

const brands = assets as Record<string, { file?: string }>

/** Decorative artwork: the adjacent source name supplies the accessible label. */
export function SourceLogo({ id, fallback: Fallback }: { id: string; fallback: LucideIcon }) {
  const [failed, setFailed] = useState(false)
  const file = brands[id]?.file
  return (
    <span className="source-logo" data-branded={Boolean(file && !failed)} aria-hidden="true">
      {file && !failed ? (
        <img src={`${import.meta.env.BASE_URL}source-logos/${file}`} alt="" width={24} height={24}
          loading="lazy" decoding="async" onError={() => setFailed(true)} />
      ) : <Fallback className="size-4" />}
    </span>
  )
}
