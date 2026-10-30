import { Check, Minus } from "lucide-react"
import {
  Popover,
  PopoverContent,
  PopoverDescription,
  PopoverTitle,
  PopoverTrigger
} from "@/components/ui/popover"
import type { SharedRecord } from "@/lib/records"
import { coreFacts, recordName } from "./facts"

export function RecordCompleteness({ record }: { record: SharedRecord }) {
  const facts = coreFacts(record)
  const count = facts.filter((fact) => fact.present).length
  return (
    <Popover>
      <PopoverTrigger
        className="record-completeness"
        aria-label={`Data completeness for ${recordName(record)}: ${count}/${facts.length} core facts present`}
      >
        <span className="record-completeness-meter" aria-hidden="true">
          {facts.map((fact, index) => (
            <span key={index} data-present={fact.present} />
          ))}
        </span>
        <span>
          {count}/{facts.length}
          <span className="sr-only"> core facts present</span>
        </span>
      </PopoverTrigger>
      <PopoverContent align="end" className="record-completeness-popover">
        <PopoverTitle>Core facts for {recordName(record)}</PopoverTitle>
        <PopoverDescription>
          {count} of {facts.length} core facts present. Presence does not verify
          accuracy, freshness, or fit.
        </PopoverDescription>
        <ul>
          {facts.map((fact) => (
            <li key={fact.label}>
              <span>{fact.label}</span>
              <span>
                {fact.present ? (
                  <Check aria-hidden="true" />
                ) : (
                  <Minus aria-hidden="true" />
                )}
                {fact.present ? "Present" : "Missing"}
              </span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  )
}
