import { useQuery } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import {
  ArrowRight,
  ArrowUpRight,
  Building2,
  Check,
  CheckCircle2,
  CircleHelp,
  Clock3,
  FileSpreadsheet,
  MailCheck,
  MoreHorizontal,
  RefreshCw,
  Sparkles,
  Table2,
  Upload,
  Users
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { useAuth } from "@/lib/auth-context"
import { recordAPI } from "@/lib/records"
import { parseRecordOverview, relativeTime } from "@/lib/record-overview"
import {
  ErrorState,
  PageHeader,
  SectionHeading,
  StatusBadge
} from "@/design-system/patterns/workbench"
import "./overview.css"

const number = new Intl.NumberFormat()
function executionTone(status: string) {
  return status === "failed"
    ? ("danger" as const)
    : status === "running"
      ? ("accent" as const)
      : status === "complete"
        ? ("positive" as const)
        : ("neutral" as const)
}
export default function OverviewPage() {
  const { activeWorkspaceId, workspaces } = useAuth()
  const workspace = workspaces.find((item) => item.id === activeWorkspaceId)
  const query = useQuery({
    queryKey: ["record-overview", activeWorkspaceId],
    queryFn: async ({ signal }) =>
      parseRecordOverview(
        await recordAPI<unknown>("records/summary", undefined, "GET", signal)
      ),
    staleTime: 15000,
    refetchInterval: 30000
  })
  const data = query.data
  const gaps = data ? data.people - data.people_with_email : 0
  const coverage =
    data && data.people > 0
      ? Math.round((data.verified_emails / data.people) * 100)
      : null
  return (
    <div className="workbench-page overview-page">
      <PageHeader
        title="Workspace overview"
        description="Your accounts, people, and enrichment work. All connected."
        actions={
          <Button
            onClick={() => void query.refetch()}
            variant="outline"
            size="icon"
            aria-label="Refresh workspace overview"
            disabled={query.isFetching}
          >
            <RefreshCw className={query.isFetching ? "animate-spin" : ""} />
          </Button>
        }
      />
      {!data && query.isError ? (
        <ErrorState
          title="Your overview is temporarily unavailable"
          error={query.error}
          retry={() => void query.refetch()}
        />
      ) : !data ? (
        <div
          className="overview-content"
          aria-label="Loading workspace overview"
          role="status"
        >
          <div className="overview-metrics">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton className="h-32 rounded-xl" key={i} />
            ))}
          </div>
          <Skeleton className="h-80 rounded-xl" />
          <span className="sr-only">Loading your workspace…</span>
        </div>
      ) : (
        <div className="overview-content">
          {query.isError && (
            <div className="overview-notice" role="alert">
              The latest update failed. Showing the last successful summary.{" "}
              <button onClick={() => void query.refetch()}>Try again</button>
            </div>
          )}
          <div className="overview-context">
            <span>
              <span className="overview-workspace-dot" />{" "}
              {workspace?.name || "Workspace overview"}
            </span>
            <span>Updated {relativeTime(data.sampled_at).toLowerCase()}</span>
          </div>
          <div className="overview-metrics">
            {[
              {
                label: "Companies",
                value: data.companies,
                icon: Building2,
                sub: "Accounts in your warehouse",
                to: "/companies"
              },
              {
                label: "People",
                value: data.people,
                icon: Users,
                sub: "Independent, connected records",
                to: "/people"
              },
              {
                label: "Verified work emails",
                value: data.verified_emails,
                icon: MailCheck,
                sub:
                  coverage === null
                    ? "Add people to measure coverage"
                    : `${coverage}% of people · explicit verification`,
                to: "/people"
              },
              {
                label: "Running workbooks",
                value: data.running_workbooks,
                icon: Table2,
                sub: data.running_workbooks
                  ? "Work is progressing in the background"
                  : "Ready when you are",
                to: "/workbooks"
              }
            ].map((metric) => (
              <Link
                to={metric.to}
                className="overview-metric"
                key={metric.label}
              >
                <div className="overview-metric-label">
                  <span>{metric.label}</span>
                  <metric.icon aria-hidden="true" />
                </div>
                <strong>{number.format(metric.value)}</strong>
                <div className="overview-metric-foot">
                  <span>{metric.sub}</span>
                  <ArrowUpRight aria-hidden="true" />
                </div>
              </Link>
            ))}
          </div>
          <section
            className="overview-next-step"
            aria-labelledby="next-step-heading"
          >
            <div className="overview-next-step-icon">
              <FileSpreadsheet aria-hidden="true" />
            </div>
            <div>
              <h2 id="next-step-heading">
                {data.companies
                  ? "Add your next company list."
                  : "Bring your first company list."}
              </h2>
              <p>
                Import a Clay table or CSV. Keep your companies, people, and
                their history connected.
              </p>
            </div>
            <Link to="/companies?import=1" className="overview-primary-link">
              <Upload aria-hidden="true" />
              Import records
              <ArrowRight aria-hidden="true" />
            </Link>
          </section>
          <div className="overview-columns">
            <section className="workbench-panel overview-workbooks">
              <SectionHeading
                title="Recent workbooks"
                description="Your recent enrichment and research workbooks."
                action={
                  <Link to="/workbooks" className="workbench-text-action">
                    View all
                    <ArrowUpRight />
                  </Link>
                }
              />
              {data.workbooks.length ? (
                <div className="overview-workbook-list">
                  {data.workbooks.map((workbook) => (
                    <Link
                      to={`/workbooks/${workbook.id}`}
                      className="overview-workbook"
                      key={workbook.id}
                    >
                      <span className="overview-workbook-icon">
                        <Table2 aria-hidden="true" />
                      </span>
                      <div>
                        <strong>{workbook.name}</strong>
                        <span>
                          {number.format(workbook.total_rows)} records{" "}
                          <span aria-hidden="true">·</span>{" "}
                          {relativeTime(workbook.updated_at)}
                        </span>
                      </div>
                      <StatusBadge tone={executionTone(workbook.status)} dot>
                        {workbook.status === "complete"
                          ? "Complete"
                          : workbook.status.charAt(0).toUpperCase() +
                            workbook.status.slice(1)}
                      </StatusBadge>
                      <ArrowUpRight aria-hidden="true" />
                    </Link>
                  ))}
                </div>
              ) : (
                <div className="overview-workbook-empty">
                  <Table2 aria-hidden="true" />
                  <h3>Make your first workbook</h3>
                  <p>
                    Select companies or people, choose the facts you need, and
                    run a small enrichment sample.
                  </p>
                  <Link to="/companies" className="workbench-text-action">
                    Start with companies
                    <ArrowRight />
                  </Link>
                </div>
              )}
            </section>
            <aside className="overview-sidebar">
              <section className="workbench-panel overview-attention">
                <SectionHeading title="Needs attention" />
                {data.failed_workbooks > 0 && (
                  <Link
                    to="/workbooks"
                    className="overview-attention-item"
                    data-tone="warning"
                  >
                    <Clock3 aria-hidden="true" />
                    <span>
                      <strong>
                        {number.format(data.failed_workbooks)} workbooks need
                        review
                      </strong>
                      <span>Inspect the run before trying again.</span>
                    </span>
                    <ArrowUpRight aria-hidden="true" />
                  </Link>
                )}
                {gaps > 0 && (
                  <Link
                    to="/people?filter=needs_email"
                    className="overview-attention-item"
                  >
                    <Users aria-hidden="true" />
                    <span>
                      <strong>
                        {number.format(gaps)} people need a work email
                      </strong>
                      <span>Find the missing connection.</span>
                    </span>
                    <ArrowUpRight aria-hidden="true" />
                  </Link>
                )}
                {data.companies_needing_domain > 0 && (
                  <Link
                    to="/companies?filter=needs_domain"
                    className="overview-attention-item"
                  >
                    <Building2 aria-hidden="true" />
                    <span>
                      <strong>
                        {number.format(data.companies_needing_domain)} companies
                        need a domain
                      </strong>
                      <span>Give research a reliable starting point.</span>
                    </span>
                    <ArrowUpRight aria-hidden="true" />
                  </Link>
                )}
                {!data.failed_workbooks &&
                  !gaps &&
                  !data.companies_needing_domain && (
                    <div className="overview-all-clear">
                      <CheckCircle2 aria-hidden="true" />
                      <strong>You’re up to date.</strong>
                      <p>
                        No missing domains, work-email gaps, or failed workbooks
                        in this summary.
                      </p>
                    </div>
                  )}
              </section>
              <section className="workbench-panel overview-shortcuts">
                <SectionHeading title="Workspace tools" />
                <Link to="/sources">
                  <Sparkles aria-hidden="true" />
                  <span>Connect your data sources</span>
                  <ArrowUpRight aria-hidden="true" />
                </Link>
                <Link to="/templates">
                  <FileSpreadsheet aria-hidden="true" />
                  <span>Explore workbook templates</span>
                  <ArrowUpRight aria-hidden="true" />
                </Link>
                <Link to="/chat">
                  <CircleHelp aria-hidden="true" />
                  <span>Work with your assistant</span>
                  <ArrowUpRight aria-hidden="true" />
                </Link>
              </section>
            </aside>
          </div>
          {data.imports.length > 0 && (
            <section className="workbench-panel overview-imports">
              <SectionHeading
                title="Recently brought into your workspace"
                description="Source-scoped imports keep your records connected on the next update."
              />
              <div className="overview-import-list">
                {data.imports.map((batch, index) => (
                  <div
                    className="overview-import"
                    key={`${batch.source}:${batch.created_at}:${index}`}
                  >
                    <span className="overview-import-icon">
                      <Check aria-hidden="true" />
                    </span>
                    <div>
                      <strong>{batch.source.replace(/^import:/, "")}</strong>
                      <span>
                        {number.format(batch.imported)} accepted
                        {batch.rejected
                          ? ` · ${number.format(batch.rejected)} rejected`
                          : " · no rejected rows"}
                      </span>
                    </div>
                    <time dateTime={batch.created_at}>
                      {relativeTime(batch.created_at)}
                    </time>
                    <MoreHorizontal aria-hidden="true" />
                  </div>
                ))}
              </div>
            </section>
          )}
        </div>
      )}
    </div>
  )
}
