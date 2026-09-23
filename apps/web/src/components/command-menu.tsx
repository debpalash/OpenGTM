import { useCallback, useEffect, useRef, useState } from "react"
import { OPEN_COMMAND_MENU_EVENT } from "@/components/app-shell/navigation"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import type { CommandMenuContent } from "./command-menu-content"

// Keep global shortcuts available without loading cmdk or querying leads on boot.
export function CommandMenu() {
  const [open, setOpen] = useState(false)
  const [Content, setContent] = useState<typeof CommandMenuContent | null>(null)
  const [failed, setFailed] = useState(false)
  const pending = useRef<Promise<void> | null>(null)
  const load = useCallback(() => {
    if (pending.current) return
    setFailed(false)
    pending.current = import("./command-menu-content")
      .then(module => { setContent(() => module.CommandMenuContent) })
      .catch(() => { pending.current = null; setFailed(true) })
  }, [])

  useEffect(() => {
    const handler = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault()
        setOpen(previous => !previous)
        load()
      }
    }
    const show = () => { setOpen(true); load() }
    window.addEventListener("keydown", handler)
    window.addEventListener(OPEN_COMMAND_MENU_EVENT, show)
    return () => {
      window.removeEventListener("keydown", handler)
      window.removeEventListener(OPEN_COMMAND_MENU_EVENT, show)
    }
  }, [load])

  // One dialog shell for both states: the lazy module renders only the command
  // list inside it, so loading → loaded never swaps the window (which replayed
  // the open animation and flashed a placeholder window).
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="top-1/3 translate-y-0 overflow-hidden rounded-xl! p-0" showCloseButton={false}>
        {Content ? (
          <>
            <DialogHeader className="sr-only">
              <DialogTitle>Command Palette</DialogTitle>
              <DialogDescription>Search for a command to run...</DialogDescription>
            </DialogHeader>
            <Content open={open} setOpen={setOpen} />
          </>
        ) : (
          <div className="grid gap-3 p-4">
            <DialogHeader>
              <DialogTitle>Command Palette</DialogTitle>
              <DialogDescription role={failed ? "alert" : "status"}>
                {failed ? "Could not load commands. Close this dialog and save any pending edits before reloading the page." : "Loading commands…"}
              </DialogDescription>
            </DialogHeader>
            {failed && <Button onClick={() => window.location.reload()}>Reload page</Button>}
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
