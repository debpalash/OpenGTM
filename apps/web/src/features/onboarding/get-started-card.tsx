import { useEffect, useRef, useState, useSyncExternalStore } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link } from "react-router-dom"
import { ArrowRight, Check, Circle, Loader2, X } from "lucide-react"
import { Button, buttonVariants } from "@/components/ui/button"
import { StatusBadge } from "@/design-system/patterns/workbench"
import { useAuth } from "@/lib/auth-context"
import { fetchOnboarding, loadSampleWorkbook, type OnboardingState } from "./onboarding-client"
import {
  DISMISS_EVENT,
  buildSteps,
  describeChange,
  progressLabel,
  readDismissed,
  sampleLoadedMessage,
  showChecklist,
  writeDismissed
} from "./onboarding-steps"
import "./get-started.css"

function subscribeDismissal(notify: () => void) {
  window.addEventListener(DISMISS_EVENT, notify)
  window.addEventListener("storage", notify)
  return () => {
    window.removeEventListener(DISMISS_EVENT, notify)
    window.removeEventListener("storage", notify)
  }
}

/**
 * "Get started" checklist at the top of the overview. Step state comes from the server (computed from real
 * workspace data), so it is never stale after the user does the work elsewhere. The live region is always
 * mounted so the last step finishing, or the card being dismissed, is still announced as the card goes away.
 */
export function GetStartedCard() {
  const { activeWorkspaceId } = useAuth()
  const queryClient = useQueryClient()
  const queryKey = ["onboarding", activeWorkspaceId]
  const dismissed = useSyncExternalStore(subscribeDismissal, () => readDismissed(), () => false)
  const query = useQuery({
    queryKey,
    queryFn: ({ signal }) => fetchOnboarding(signal),
    staleTime: 10000,
    refetchInterval: 30000
  })
  const [announcement, setAnnouncement] = useState("")
  const [sampleError, setSampleError] = useState("")
  const previous = useRef<OnboardingState | undefined>(undefined)

  useEffect(() => {
    const next = query.data
    if (!next) return
    if (previous.current) {
      const message = describeChange(previous.current, next)
      if (message) setAnnouncement(message)
    }
    previous.current = next
  }, [query.data])

  const sample = useMutation({
    mutationFn: () => loadSampleWorkbook(),
    onMutate: () => {
      setSampleError("")
      setAnnouncement("Loading the sample workbook.")
    },
    onSuccess: (result) => {
      setAnnouncement(sampleLoadedMessage(result.created))
      void queryClient.invalidateQueries({ queryKey })
      void queryClient.invalidateQueries({ queryKey: ["record-overview"] })
    },
    onError: (error: Error) => {
      setSampleError(error.message)
      setAnnouncement(error.message)
    }
  })

  const dismiss = () => {
    writeDismissed(true)
    setAnnouncement("Setup guide hidden. Choose Show setup guide in the command menu to bring it back.")
  }

  const state = query.data
  const visible = showChecklist(state, dismissed)
  const steps = state ? buildSteps(state) : []
  // One filled button per view: only the first unfinished step gets it.
  const nextId = steps.find((step) => !step.done)?.id
  return (
    <>
      <div role="status" aria-live="polite" aria-atomic="true" className="sr-only">
        {announcement}
      </div>
      {visible && state && (
        <section className="workbench-panel getstarted" aria-labelledby="getstarted-title">
          <div className="getstarted-head">
            <div>
              <h2 id="getstarted-title">Get started</h2>
              <p>Four steps to your first enriched list. Each one links to the place to do it.</p>
            </div>
            <div className="getstarted-head-side">
              <p className="getstarted-progress">{progressLabel(state)}</p>
              <Button variant="ghost" onClick={dismiss}>
                <X aria-hidden="true" />
                Dismiss
              </Button>
            </div>
          </div>
          <div className="getstarted-bar" aria-hidden="true">
            <span style={{ width: `${state.total ? (state.done_count / state.total) * 100 : 0}%` }} />
          </div>
          <ol className="getstarted-steps">
            {steps.map((step, index) => (
              <li key={step.id} className="getstarted-step" data-done={step.done || undefined}>
                <span className="getstarted-mark" aria-hidden="true">
                  {step.done ? <Check /> : <Circle />}
                </span>
                <div className="getstarted-body">
                  <div className="getstarted-title">
                    <strong>
                      <span aria-hidden="true">{index + 1}. </span>
                      {step.title}
                    </strong>
                    <StatusBadge tone={step.done ? "positive" : "neutral"}>{step.done ? "Done" : "To do"}</StatusBadge>
                  </div>
                  <p>{step.why}</p>
                  {step.id === "data" && sampleError && (
                    <p className="getstarted-error" role="alert">
                      {sampleError}
                    </p>
                  )}
                </div>
                {step.actions.length > 0 && (
                  <div className="getstarted-actions">
                    {step.actions.map((action, i) =>
                      action.kind === "link" ? (
                        // A real link (not a button role) styled like a button: it navigates.
                        <Link key={action.label} to={action.to} className={buttonVariants({ variant: i === 0 && step.id === nextId ? "default" : "outline" })}>
                          {action.label}
                          <ArrowRight aria-hidden="true" />
                        </Link>
                      ) : (
                        <Button
                          key={action.label}
                          variant="outline"
                          disabled={sample.isPending}
                          onClick={() => sample.mutate()}
                        >
                          {sample.isPending && <Loader2 className="animate-spin" aria-hidden="true" />}
                          {action.label}
                        </Button>
                      )
                    )}
                  </div>
                )}
              </li>
            ))}
          </ol>
        </section>
      )}
    </>
  )
}
