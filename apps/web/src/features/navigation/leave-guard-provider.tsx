import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode
} from "react"
import { useBlocker } from "react-router-dom"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import { LeaveGuard, type LeaveBlock } from "./leave-guard"

const Context = createContext<LeaveGuard | null>(null)
function useGuard() {
  const guard = useContext(Context)
  if (!guard) throw new Error("LeaveGuardProvider is required")
  return guard
}
export function useLeaveConfirmation() {
  const guard = useGuard()
  return { request: guard.request, cancel: guard.cancel }
}
export function useLeaveGuard({ dirty, busy, message, busyLabel }: LeaveBlock) {
  const guard = useGuard()
  const id = useId()
  const released = useRef(false)
  useEffect(() => {
    if (!released.current) guard.set(id, { dirty, busy, message, busyLabel })
    return () => guard.set(id, null)
  }, [guard, id, dirty, busy, message, busyLabel])
  return useCallback(() => {
    released.current = true
    guard.set(id, null)
  }, [guard, id])
}
export function LeaveGuardProvider({ children }: { children: ReactNode }) {
  const [guard] = useState(() => new LeaveGuard())
  const prompt = useSyncExternalStore(guard.subscribe, guard.getSnapshot)
  const blocker = useBlocker(guard.isProtected)
  const reviewingNavigation = useRef(false)
  useEffect(() => {
    if (blocker.state !== "blocked" || reviewingNavigation.current) return
    reviewingNavigation.current = true
    void guard.request("Discard your changes").then((leave) => {
      reviewingNavigation.current = false
      if (leave) blocker.proceed()
      else blocker.reset()
    })
  }, [blocker, guard])
  useEffect(() => {
    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (!guard.isProtected()) return
      event.preventDefault()
      event.returnValue = ""
    }
    window.addEventListener("beforeunload", beforeUnload)
    return () => window.removeEventListener("beforeunload", beforeUnload)
  }, [guard])
  return (
    <Context.Provider value={guard}>
      {children}
      <Dialog
        open={!!prompt}
        onOpenChange={(open) => {
          if (!open) guard.answer(false)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {prompt?.busy
                ? prompt.busyLabel || "Save in progress"
                : prompt
                  ? `${prompt.action}?`
                  : "Unsaved changes"}
            </DialogTitle>
            <DialogDescription>
              {prompt?.busy
                ? "Wait until the result is confirmed before continuing. Your current workspace stays open."
                : prompt?.dirtyCount
                  ? prompt.messages.length
                    ? prompt.messages.join(" ")
                    : "You have unsaved changes in this workspace. Keep editing, or discard them to continue."
                  : "Your changes are saved. You can continue when you are ready."}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => guard.answer(false)}>
              Keep editing
            </Button>
            <Button
              variant={prompt?.dirtyCount ? "destructive" : "default"}
              disabled={prompt?.busy}
              onClick={() => guard.answer(true)}
            >
              {prompt?.dirtyCount ? "Discard changes" : "Continue"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Context.Provider>
  )
}
