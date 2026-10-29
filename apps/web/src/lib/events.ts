// GET /api/events (SSE) connection helper.
//
// EventSource cannot send an Authorization header. The Go API issues a short-lived,
// single-use ticket (POST /api/events/ticket) so the access token never rides in the
// URL. A ticket is good for one connection only, so every (re)connect fetches a fresh
// one and the browser's built-in retry (which would reuse the spent URL) is replaced
// by our own. When no ticket can be had (the Python API has no such route), or a
// ticket-based stream fails twice in a row before it ever opened (a canary split sent
// the GET to a stack that does not know tickets), we fall back to the legacy ?token=
// query for the rest of the page session.

import { authQuery } from "./auth"

let preferLegacy = false
let ticketFailures = 0

async function ticketQuery(): Promise<string | null> {
  if (preferLegacy) return null
  try {
    const res = await fetch("/api/events/ticket", { method: "POST" })
    if (!res.ok) return null
    const body = (await res.json()) as { ticket?: unknown }
    if (typeof body.ticket !== "string" || !body.ticket) return null
    return `?${new URLSearchParams({ ticket: body.ticket }).toString()}`
  } catch {
    return null
  }
}

export interface EventStreamHandlers {
  onMessage: (event: MessageEvent<string>) => void
  onOpen?: () => void
  onError?: () => void
  /** Reconnect (with a fresh ticket) this many ms after an error; omit to stay closed. */
  reconnectMs?: number
}

/** Open an SSE stream on `url` (default /api/events). Returns a function that closes it. */
export function openEventStream(handlers: EventStreamHandlers, url = "/api/events"): () => void {
  let closed = false
  let es: EventSource | null = null
  let timer: ReturnType<typeof setTimeout> | null = null

  const connect = async () => {
    const ticket = await ticketQuery()
    if (closed) return
    const usedTicket = ticket !== null
    let opened = false
    try {
      es = new EventSource(`${url}${ticket ?? authQuery()}`)
    } catch {
      handlers.onError?.()
      return
    }
    es.onopen = () => {
      opened = true
      if (usedTicket) ticketFailures = 0
      handlers.onOpen?.()
    }
    es.onmessage = handlers.onMessage
    es.onerror = () => {
      es?.close()
      es = null
      if (usedTicket && !opened && ++ticketFailures >= 2) preferLegacy = true
      handlers.onError?.()
      if (!closed && handlers.reconnectMs !== undefined) {
        timer = setTimeout(() => void connect(), handlers.reconnectMs)
      }
    }
  }

  void connect()
  return () => {
    closed = true
    if (timer) clearTimeout(timer)
    es?.close()
  }
}
