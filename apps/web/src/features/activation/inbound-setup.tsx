import { useLayoutEffect, useRef, useState } from "react"
import { useInfiniteQuery } from "@tanstack/react-query"
import { Activity, Copy, KeyRound, RefreshCw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import { getActiveWorkspace } from "@/lib/auth"
import type { AudienceDestination } from "@/lib/api"
import {
  useLeaveConfirmation,
  useLeaveGuard
} from "@/features/navigation/leave-guard-provider"
import { fetchInboundPage, rotateInboundToken } from "./inbound-client"

/** Token lifetime and receipt reads are independent: a successful rotation
 * remains recoverable even when the history request fails. Secrets stay in memory. */
export function InboundSetup({
  destination,
  canWrite
}: {
  destination: AudienceDestination
  canWrite: boolean
}) {
  const [open, setOpen] = useState(false)
  const [review, setReview] = useState(false)
  const [token, setToken] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const [stored, setStored] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [copyError, setCopyError] = useState("")
  const rotating = useRef(false)
  const resultHeading = useRef<HTMLHeadingElement>(null)
  useLayoutEffect(() => {
    if (!open || review) return
    const frame = requestAnimationFrame(() =>
      resultHeading.current?.focus({ preventScroll: true })
    )
    return () => cancelAnimationFrame(frame)
  }, [open, review])
  const { request } = useLeaveConfirmation()
  useLeaveGuard({
    dirty: !!token && !copied && !stored,
    busy,
    busyLabel: "Token rotation in progress",
    message:
      "The new inbound token has not been copied. It is shown only once; copy it before leaving, or discard it to continue."
  })
  const history = useInfiniteQuery({
    queryKey: ["inbound-receipts", getActiveWorkspace(), destination.id],
    queryFn: ({ pageParam, signal }) =>
      fetchInboundPage(destination.id, pageParam, signal),
    initialPageParam: null as string | null,
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: open && !review
  })
  const receipts = [
    ...new Map(
      history.data?.pages
        .flatMap((page) => page.receipts)
        .map((receipt) => [receipt.id, receipt])
    ).values()
  ]
  async function close() {
    if (rotating.current) return
    if (token && !copied && !stored && !(await request("Close token setup")))
      return
    setOpen(false)
    setToken(null)
    setCopied(false)
    setStored(false)
    setError("")
    setCopyError("")
  }
  async function rotate() {
    if (rotating.current || !canWrite) return
    rotating.current = true
    setBusy(true)
    setError("")
    try {
      const next = await rotateInboundToken(destination.id)
      setToken(next)
      setCopied(false)
      setStored(false)
      setReview(false)
    } catch (cause) {
      setError(
        cause instanceof Error
          ? cause.message
          : "Token rotation could not be confirmed. Review the setup before trying again."
      )
    } finally {
      rotating.current = false
      setBusy(false)
    }
  }
  async function copy() {
    if (!token) return
    setCopyError("")
    try {
      await navigator.clipboard.writeText(token)
      setCopied(true)
    } catch {
      setCopied(false)
      setCopyError(
        "The token could not be copied. Try again, or select and copy the full token below."
      )
    }
  }
  return (
    <>
      <Button
        variant="outline"
        size="sm"
        onClick={() => {
          setReview(false)
          setOpen(true)
        }}
      >
        <Activity aria-hidden="true" className="size-3" /> Inbound history
      </Button>
      <Button
        variant="outline"
        size="sm"
        disabled={!canWrite}
        onClick={() => {
          setReview(true)
          setOpen(true)
        }}
      >
        <KeyRound aria-hidden="true" className="size-3" /> Rotate token
      </Button>
      <Dialog
        open={open}
        onOpenChange={(value) => {
          if (!value) void close()
        }}
      >
        <DialogContent className="max-h-[calc(100dvh-24px)] grid-rows-[auto_minmax(0,1fr)_auto] sm:max-w-xl">
          <DialogHeader>
            <DialogTitle ref={resultHeading} tabIndex={-1}>
              {review ? "Rotate inbound token?" : "Inbound callbacks"}
            </DialogTitle>
            <DialogDescription>
              {destination.name} · {destination.destination_type}
            </DialogDescription>
          </DialogHeader>
          {review ? (
            <div className="space-y-3 text-sm">
              {!canWrite && (
                <p className="text-muted-foreground">
                  View only. Activation write permission is required to rotate
                  this token.
                </p>
              )}
              <p>
                The current token will stop working immediately. Update your CRM
                callback with the new token after rotating it.
              </p>
              <p className="text-muted-foreground">
                The new token is shown once. Keep this setup open until you have
                copied it to your CRM secret manager.
              </p>
              {error && (
                <p role="alert" className="text-destructive">
                  {error}
                </p>
              )}
            </div>
          ) : (
            <div className="space-y-3 min-h-0 overflow-y-auto">
              <code className="block break-all text-xs">
                POST {window.location.origin}/api/audience-destinations/inbound/
                {destination.id}
              </code>
              {token && (
                <div className="space-y-2 rounded-md border border-border bg-muted p-3">
                  <p className="text-sm font-medium">
                    New inbound token · shown once
                  </p>
                  <p className="text-xs text-muted-foreground">
                    Store this Bearer token in your CRM webhook secret manager.
                  </p>
                  <code
                    className="block select-all break-all text-xs"
                    aria-label="New inbound token"
                  >
                    {token}
                  </code>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void copy()}
                  >
                    <Copy aria-hidden="true" className="size-3" /> Copy token
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setStored(true)}
                  >
                    I stored this token
                  </Button>
                  {stored && !copied && (
                    <p
                      role="status"
                      className="text-xs text-[var(--gtm-success)]"
                    >
                      Token marked as stored.
                    </p>
                  )}
                  {copied && (
                    <p
                      role="status"
                      className="text-xs text-[var(--gtm-success)]"
                    >
                      Token copied.
                    </p>
                  )}
                  {copyError && (
                    <p role="alert" className="text-xs text-destructive">
                      {copyError}
                    </p>
                  )}
                </div>
              )}
              <p className="text-xs text-muted-foreground">
                Send external_event_id, optional external_record_id or lead_id,
                and fields. Only allowlisted fields are accepted; retries are
                idempotent.
              </p>
              {history.isPending && (
                <p role="status" className="text-sm text-muted-foreground">
                  Loading inbound history…
                </p>
              )}
              {history.isError && (
                <div role="alert" className="space-y-2 text-sm">
                  <p className="text-destructive">
                    History could not be loaded.{" "}
                    {token
                      ? "Your new token is still available above."
                      : "Try again to load callbacks."}
                  </p>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={history.isFetching}
                    onClick={() =>
                      void (history.isFetchNextPageError
                        ? history.fetchNextPage()
                        : history.refetch())
                    }
                  >
                    Try history again
                  </Button>
                </div>
              )}
              <div className="space-y-1">
                {receipts.map((receipt) => (
                  <div
                    key={receipt.id}
                    className="flex flex-wrap items-center justify-between gap-2 rounded border p-2 text-xs"
                  >
                    <span className="min-w-0 break-all">
                      {receipt.external_event_id} · lead{" "}
                      {receipt.lead_id ?? "unmatched"} ·{" "}
                      {receipt.applied_fields.length} fields
                    </span>
                    <Badge
                      variant={
                        receipt.status === "unmatched"
                          ? "destructive"
                          : "outline"
                      }
                    >
                      {receipt.status}
                    </Badge>
                  </div>
                ))}
                {!history.isPending && !history.isError && !receipts.length && (
                  <p className="text-xs text-muted-foreground">
                    No inbound callbacks received yet.
                  </p>
                )}
                {history.hasNextPage && (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={history.isFetching}
                    onClick={() => void history.fetchNextPage()}
                  >
                    {history.isFetchingNextPage && (
                      <RefreshCw
                        aria-hidden="true"
                        className="size-3 animate-spin"
                      />
                    )}{" "}
                    Load older callbacks
                  </Button>
                )}
              </div>
            </div>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => void close()}
            >
              {review ? "Cancel" : "Close setup"}
            </Button>
            {review && (
              <Button
                disabled={busy || !canWrite}
                aria-busy={busy}
                onClick={() => void rotate()}
              >
                {busy && (
                  <RefreshCw
                    aria-hidden="true"
                    className="size-3 animate-spin"
                  />
                )}{" "}
                Rotate token
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
