import { ProviderIcon } from "./provider-icon"
import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { useAuth } from "@/lib/auth-context"
import { toast } from "sonner"
import {
  ExternalLink,
  Check,
  X,
  Loader2,
  TestTube2,
  Eye,
  EyeOff,
  Star,
  CircleCheck,
  CircleX
} from "lucide-react"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle
} from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from "@/components/ui/dialog"
import { useQueryClient } from "@tanstack/react-query"
import { queryKeys } from "@/lib/query-client"
import { useLeaveGuard } from "@/features/navigation/leave-guard-provider"
import {
  ProviderWriteError,
  fetchProviderSettings,
  updateProvider,
  testProvider,
  type Provider,
  type ProviderUpdate,
  type ProviderSettings
} from "./provider-client"

export function ProviderCard({ provider }: { provider: Provider }) {
  const { user } = useAuth()
  const canManage = user?.is_admin === true
  const [apiKey, setApiKey] = useState("")
  const [modelDraft, setModelDraft] = useState({
    value: provider.model,
    baseline: provider.model
  })
  const model = modelDraft.value
  useEffect(() => {
    setModelDraft((current) =>
      current.value === current.baseline
        ? { value: provider.model, baseline: provider.model }
        : current
    )
  }, [provider.model])
  const [showKey, setShowKey] = useState(false)
  const [discardOpen, setDiscardOpen] = useState(false)
  const discardTrigger = useRef<HTMLButtonElement>(null)
  const discarded = useRef(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{
    ok: boolean
    message: string
  } | null>(null)
  const [saving, setSaving] = useState(false)
  const [checking, setChecking] = useState(false)
  const [saveError, setSaveError] = useState("")
  const [receipt, setReceipt] = useState("")
  const receiptRef = useRef<HTMLParagraphElement>(null)
  const errorRef = useRef<HTMLParagraphElement>(null)
  useLayoutEffect(() => {
    if (saveError) errorRef.current?.focus()
    else if (receipt) receiptRef.current?.focus()
  }, [saveError, receipt])
  const [settingDefault, setSettingDefault] = useState(false)
  const [unknown, setUnknown] = useState<{
    kind: "save" | "default"
    hasKey: boolean
  } | null>(null)
  const active = useRef(false)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const dirty = !!apiKey || model !== modelDraft.baseline
  const busy = saving || testing || settingDefault || checking
  useLeaveGuard({
    dirty: dirty || !!unknown,
    busy,
    message: unknown
      ? "A provider save outcome is unknown. Leaving discards this draft, but does not undo any saved settings."
      : undefined
  })
  const qc = useQueryClient()

  const handleTest = async () => {
    if (active.current || !canManage || dirty || unknown) return
    active.current = true
    setTesting(true)
    setTestResult(null)
    try {
      const result = await testProvider(provider.id)
      if (alive.current) setTestResult(result)
    } catch (error) {
      if (alive.current)
        setTestResult({
          ok: false,
          message:
            error instanceof Error
              ? error.message
              : "Provider test could not be completed. Try again."
        })
    } finally {
      active.current = false
      if (alive.current) setTesting(false)
    }
  }

  const write = async (kind: "save" | "default") => {
    if (
      active.current ||
      !canManage ||
      (unknown && unknown.kind !== kind) ||
      (kind === "save" && !dirty) ||
      (kind === "default" && dirty)
    )
      return
    const input: ProviderUpdate =
      kind === "default"
        ? { set_default: true as const }
        : {
            ...(apiKey ? { api_key: apiKey } : {}),
            ...(model !== modelDraft.baseline
              ? { model: model || provider.default_model }
              : {})
          }
    active.current = true
    if (kind === "save") setSaving(true)
    else setSettingDefault(true)
    setSaveError("")
    setReceipt("")
    setTestResult(null)
    try {
      await updateProvider(provider.id, input)
      if (!alive.current) return
      const effectiveModel = input.model
      qc.setQueryData<ProviderSettings>(queryKeys.providers, (current) =>
        current
          ? {
              ...current,
              default_provider:
                kind === "default" ? provider.id : current.default_provider,
              providers: current.providers.map((item) => ({
                ...item,
                ...(kind === "default"
                  ? { is_default: item.id === provider.id }
                  : {}),
                ...(item.id === provider.id
                  ? {
                      ...(effectiveModel !== undefined
                        ? { model: effectiveModel }
                        : {}),
                      ...(input.api_key
                        ? { configured: true, api_key_masked: "" }
                        : {})
                    }
                  : {})
              }))
            }
          : current
      )
      if (kind === "save") {
        setApiKey("")
        setModelDraft((current) => ({
          value: effectiveModel ?? current.value,
          baseline: effectiveModel ?? current.value
        }))
      }
      setUnknown(null)
      setReceipt(
        kind === "default"
          ? `${provider.name} is the default provider.`
          : "Changes saved. Connection has not been tested."
      )
      toast.success(
        kind === "default"
          ? `${provider.name} set as default`
          : `${provider.name} updated`
      )
      void qc.invalidateQueries({ queryKey: queryKeys.providers })
    } catch (error) {
      if (!alive.current) return
      const failure =
        error instanceof ProviderWriteError
          ? error
          : new ProviderWriteError("unknown")
      setSaveError(failure.message)
      if (failure.outcome === "unknown")
        setUnknown({ kind, hasKey: !!input.api_key })
    } finally {
      active.current = false
      if (alive.current) {
        setSaving(false)
        setSettingDefault(false)
      }
    }
  }

  const checkSaved = async () => {
    if (active.current || !unknown) return
    active.current = true
    setChecking(true)
    setSaveError("")
    try {
      const current = await fetchProviderSettings()
      if (!alive.current) return
      const saved = current.providers.find((item) => item.id === provider.id)
      if (!saved)
        throw new Error(
          "This provider is no longer available. Your draft is kept."
        )
      qc.setQueryData(queryKeys.providers, current)
      setModelDraft((draft) => ({ ...draft, baseline: saved.model }))
      if (unknown.kind === "default") {
        if (current.default_provider === provider.id) {
          setUnknown(null)
          setReceipt(`${provider.name} is currently the default provider.`)
        } else
          setReceipt(
            "Current settings loaded. This provider is not the default. Setting it again is a new request."
          )
      } else if (unknown.hasKey) {
        setReceipt(
          "Current settings loaded. The API key cannot be verified from its masked value. Saving a new key explicitly replaces the saved key."
        )
      } else {
        setUnknown(null)
        if ((model || provider.default_model) === saved.model) {
          setModelDraft({ value: saved.model, baseline: saved.model })
        }
        setReceipt(
          (model || provider.default_model) === saved.model
            ? "Saved model matches your draft. Connection has not been tested."
            : "Current saved model loaded. Your model draft is kept."
        )
      }
    } catch (error) {
      if (alive.current)
        setSaveError(
          error instanceof Error
            ? error.message
            : "Saved settings could not be checked. Your draft is kept."
        )
    } finally {
      active.current = false
      if (alive.current) setChecking(false)
    }
  }

  return (
    <>
      <Card
        role="region"
        className="provider-settings-card"
        aria-label={`${provider.name} provider settings`}
      >
        <CardHeader className="pb-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <CardTitle className="min-w-0 text-sm font-medium flex flex-wrap items-center gap-2">
              <ProviderIcon icon={provider.icon} />
              {provider.name}
              {provider.is_default && (
                <Badge variant="default" className="text-xs gap-1">
                  <Star className="size-3" /> Default
                </Badge>
              )}
            </CardTitle>
            <div className="flex items-center gap-1">
              {provider.configured ? (
                <Badge
                  variant="outline"
                  className="text-xs text-[var(--t-color-green11)] border-[var(--t-color-green7)]"
                >
                  <Check className="size-3 mr-1" /> Configured
                </Badge>
              ) : (
                <Badge
                  variant="outline"
                  className="text-xs text-muted-foreground"
                >
                  <X className="size-3 mr-1" /> Not set
                </Badge>
              )}
            </div>
          </div>
          <CardDescription className="text-xs">
            {provider.free_tier}
            <a
              href={provider.docs_url}
              target="_blank"
              rel="noopener noreferrer"
              className="ml-2 inline-flex items-center gap-0.5 hover:underline"
            >
              Docs
              <span className="sr-only">
                {" "}
                for {provider.name} (opens in a new tab)
              </span>{" "}
              <ExternalLink aria-hidden="true" className="size-3" />
            </a>
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor={`provider-${provider.id}-key`} className="text-xs">
              API Key
            </Label>
            <div className="flex items-center gap-1">
              <Input
                id={`provider-${provider.id}-key`}
                type={showKey ? "text" : "password"}
                placeholder={
                  provider.api_key_masked ||
                  (provider.configured ? "Saved key" : "Enter API key...")
                }
                autoComplete="new-password"
                spellCheck={false}
                aria-describedby={`provider-${provider.id}-key-help`}
                value={apiKey}
                disabled={busy || !canManage}
                onChange={(e) => {
                  setApiKey(e.target.value)
                  setTestResult(null)
                  setReceipt("")
                }}
                className="font-mono text-xs"
              />
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`${showKey ? "Hide" : "Show"} ${provider.name} API key`}
                onClick={() => setShowKey(!showKey)}
                aria-pressed={showKey}
                className="provider-key-reveal"
              >
                {showKey ? (
                  <EyeOff className="size-4" />
                ) : (
                  <Eye className="size-4" />
                )}
              </Button>
            </div>
            <p
              id={`provider-${provider.id}-key-help`}
              className="text-xs text-muted-foreground"
            >
              {provider.configured
                ? "Leave blank to keep the saved key."
                : "Add a key to configure this provider."}
            </p>
          </div>

          <div className="space-y-1.5">
            <Label
              htmlFor={`provider-${provider.id}-model`}
              className="text-xs"
            >
              Model
            </Label>
            <Input
              id={`provider-${provider.id}-model`}
              value={model}
              disabled={busy || !canManage}
              onChange={(e) => {
                setModelDraft((current) => ({
                  ...current,
                  value: e.target.value
                }))
                setTestResult(null)
                setReceipt("")
              }}
              placeholder={provider.default_model}
              aria-describedby={`provider-${provider.id}-model-help`}
              className="text-xs"
            />
            <p
              id={`provider-${provider.id}-model-help`}
              className="text-xs text-muted-foreground break-words"
            >
              Saved model: {modelDraft.baseline || provider.default_model}.
              Leave blank to use {provider.default_model}.
            </p>
          </div>

          <div className="flex flex-wrap items-center gap-2 pt-1">
            <Button
              size="sm"
              onClick={() => void write("save")}
              disabled={
                busy ||
                !canManage ||
                (!apiKey && model === modelDraft.baseline) ||
                unknown?.kind === "default"
              }
              aria-busy={saving}
              className="min-w-16"
            >
              {saving && <Loader2 className="size-3 animate-spin" />} Save
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={handleTest}
              disabled={
                busy || !canManage || !provider.configured || dirty || !!unknown
              }
              aria-busy={testing}
              className="min-w-16"
            >
              {testing ? (
                <Loader2 className="size-3 animate-spin" />
              ) : (
                <TestTube2 className="size-3" />
              )}{" "}
              Test
            </Button>
            {!provider.is_default && provider.configured && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => void write("default")}
                disabled={
                  busy || !canManage || dirty || unknown?.kind === "save"
                }
                aria-busy={settingDefault}
              >
                Set default
              </Button>
            )}
            {(dirty || unknown) && (
              <Button
                size="sm"
                variant="ghost"
                disabled={busy}
                ref={discardTrigger}
                onClick={() => {
                  discarded.current = false
                  setDiscardOpen(true)
                }}
              >
                Discard draft
              </Button>
            )}
          </div>

          <p className="text-xs text-muted-foreground">
            {dirty || unknown ? "Save changes before testing. " : ""}Tests use
            saved settings and may incur provider usage.
          </p>
          {unknown && (
            <div className="space-y-2 rounded-md border bg-muted/30 p-2">
              <p className="text-xs">
                Check saved settings before deciding whether to save again.
                Discarding a draft does not undo settings already saved.
              </p>
              <Button
                variant="outline"
                size="sm"
                onClick={() => void checkSaved()}
                disabled={busy}
                aria-busy={checking}
              >
                {checking && (
                  <Loader2 aria-hidden="true" className="size-3 animate-spin" />
                )}{" "}
                Check saved settings
              </Button>
            </div>
          )}
          {receipt && (
            <p
              ref={receiptRef}
              tabIndex={-1}
              role="status"
              className="text-xs text-muted-foreground"
            >
              {receipt}
            </p>
          )}
          {!canManage && (
            <p role="status" className="text-xs text-muted-foreground">
              View only. An instance admin manages provider settings.
              {apiKey || model !== modelDraft.baseline
                ? " Your draft is kept."
                : ""}
            </p>
          )}
          {saveError && (
            <p
              ref={errorRef}
              tabIndex={-1}
              role="alert"
              className="text-xs text-destructive"
            >
              {saveError}
            </p>
          )}
          {testResult && (
            <div
              role="status"
              className={`flex items-start gap-1.5 rounded-md p-2 text-xs ${testResult.ok ? "bg-[var(--t-color-green3)] text-[var(--t-color-green11)]" : "bg-destructive/10 text-destructive"}`}
            >
              {testResult.ok ? (
                <CircleCheck
                  aria-hidden="true"
                  className="mt-px size-3.5 shrink-0"
                />
              ) : (
                <CircleX
                  aria-hidden="true"
                  className="mt-px size-3.5 shrink-0"
                />
              )}
              <span className="min-w-0 break-words">{testResult.message}</span>
            </div>
          )}
        </CardContent>
      </Card>
      <Dialog open={discardOpen} onOpenChange={setDiscardOpen}>
        <DialogContent
          finalFocus={() =>
            discarded.current ? receiptRef.current : discardTrigger.current
          }
        >
          <DialogHeader>
            <DialogTitle>Discard provider draft?</DialogTitle>
            <DialogDescription>
              Discard the draft for {provider.name}. Any settings already saved
              remain in effect; this does not undo an earlier save.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDiscardOpen(false)}>
              Keep editing
            </Button>
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => {
                discarded.current = true
                setDiscardOpen(false)
                setApiKey("")
                setModelDraft((current) => ({
                  value: current.baseline,
                  baseline: current.baseline
                }))
                setUnknown(null)
                setSaveError("")
                setTestResult(null)
                setShowKey(false)
                setReceipt(
                  "Draft discarded. Any settings already saved remain in effect."
                )
                void qc.invalidateQueries({ queryKey: queryKeys.providers })
              }}
            >
              Discard draft
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
