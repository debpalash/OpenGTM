import { Link } from "react-router-dom"
import { audienceSubject, type AudienceSubject } from "./audience-subject"

export function AudienceMemberItem({ member }: { member: AudienceSubject }) {
  const subject = audienceSubject(member)
  const score =
    typeof member.snapshot.score === "number" ||
    typeof member.snapshot.score === "string"
      ? String(member.snapshot.score)
      : "—"
  const content = (
    <>
      <span className="min-w-0">
        <strong className="block truncate font-medium">{subject.name}</strong>
        <span className="block text-xs text-muted-foreground">
          {subject.kind}
          {subject.detail ? ` · ${subject.detail}` : ""}
        </span>
      </span>
      <span className="ml-3 shrink-0 text-xs text-muted-foreground">
        {score} score
      </span>
    </>
  )
  const className =
    "flex items-center justify-between rounded-md px-3 py-3 text-sm hover:bg-muted/60"
  return subject.href ? (
    <Link className={className} to={subject.href}>
      {content}
    </Link>
  ) : (
    <div className={className}>{content}</div>
  )
}
