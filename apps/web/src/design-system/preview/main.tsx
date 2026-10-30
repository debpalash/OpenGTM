import { StrictMode, useState, type ReactNode } from "react"
import { createRoot } from "react-dom/client"
import {
  Check,
  Copy,
  MoreHorizontal,
  Plus,
  RefreshCw,
  Settings,
  Upload
} from "lucide-react"
import { toast } from "sonner"
import { ThemeProvider } from "../theme/theme-provider"
import { ThemeSelect } from "../theme/theme-select"
import {
  Button,
  Checkbox,
  Field,
  IconButton,
  Input,
  Menu,
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  ModalTrigger,
  NativeSelect,
  Progress,
  ProgressLabel,
  ProgressValue,
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
  StatusBadge,
  Switch,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
  Textarea,
  Toaster,
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger
} from "../primitives"
import { ViewState } from "../patterns/workbench"
import "../../index.css"
import "./preview.css"

function Specimen({
  title,
  description,
  children
}: {
  title: string
  description: string
  children: ReactNode
}) {
  return (
    <section className="gallery-specimen">
      <div className="gallery-specimen-heading">
        <h2>{title}</h2>
        <p>{description}</p>
      </div>
      {children}
    </section>
  )
}
function Gallery() {
  const [tab, setTab] = useState("overview")
  const [enabled, setEnabled] = useState(true)
  const [checked, setChecked] = useState(false)
  const [loading, setLoading] = useState(false)
  const [modal, setModal] = useState(false)
  const [email, setEmail] = useState("")
  const [error, setError] = useState("")
  async function demonstrateSave() {
    setLoading(true)
    await new Promise((resolve) => setTimeout(resolve, 900))
    setLoading(false)
    toast.success("Preview saved. No workspace data changed.")
  }
  return (
    <TooltipProvider>
      <div className="ui-gallery">
        <header className="gallery-header">
          <a href="/" className="gallery-brand">
            OpenGTM
          </a>
          <span>Workbench design system</span>
          <ThemeSelect />
        </header>
        <main>
          <div className="gallery-heading">
            <p className="workbench-eyebrow">APPLICATION COMPONENTS</p>
            <h1>One system. Every workflow.</h1>
            <p>
              Live controls, shared states, and accessible form contracts. All
              examples use fictional data and make no backend requests.
            </p>
          </div>
          <div className="gallery-grid">
            <Specimen
              title="Actions"
              description="Stable width, visible focus, and named loading states."
            >
              <div className="gallery-row">
                <Button
                  variant="solid"
                  startIcon={<Upload aria-hidden="true" className="size-4" />}
                >
                  Import records
                </Button>
                <Button>Review enrichment</Button>
                <Button variant="ghost">Cancel</Button>
                <Button variant="solid" color="danger">
                  Delete record
                </Button>
                <Button variant="soft">Secondary action</Button>
              </div>
              <div className="gallery-row">
                <Button disabled>Unavailable</Button>
                <Button loading>Save workflow</Button>
                <Button aria-invalid="true">Invalid action</Button>
                <Button
                  variant="solid"
                  loading={loading}
                  startIcon={<Check aria-hidden="true" className="size-4" />}
                  onClick={() => void demonstrateSave()}
                >
                  Try saving
                </Button>
              </div>
            </Specimen>
            <Specimen
              title="Icon actions and help"
              description="An accessible name is required. Tooltips add context."
            >
              <div className="gallery-row">
                <IconButton label="Add company" variant="outline">
                  <Plus />
                </IconButton>
                <IconButton label="Copy company domain" variant="ghost">
                  <Copy />
                </IconButton>
                <IconButton label="Refresh companies" disabled>
                  <RefreshCw />
                </IconButton>
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <IconButton
                        label="Configure enrichment"
                        variant="outline"
                      >
                        <Settings />
                      </IconButton>
                    }
                  >
                    <Settings />
                  </TooltipTrigger>
                  <TooltipContent>
                    Choose providers and fields before running.
                  </TooltipContent>
                </Tooltip>
                <IconButton label="Invalid configuration" aria-invalid="true">
                  <Settings />
                </IconButton>
              </div>
            </Specimen>
            <Specimen
              title="Fields"
              description="Labels, help, required state, and validation stay connected."
            >
              <Field
                label="Company domain"
                description="Use the company’s primary domain."
                required
              >
                {(control) => <Input {...control} placeholder="example.test" />}
              </Field>
              <Field
                label="Work email"
                description="Used to identify this person."
                error="Enter a complete work email address."
              >
                {(control) => (
                  <Input {...control} type="email" defaultValue="ana@" />
                )}
              </Field>
              <Field
                label="API key"
                description="Workspace editors cannot reveal this value."
              >
                {(control) => (
                  <Input
                    {...control}
                    type="password"
                    value="example-only"
                    disabled
                  />
                )}
              </Field>
              <Field label="Research notes">
                {(control) => (
                  <Textarea
                    {...control}
                    defaultValue="Confirm the current employer before activation."
                  />
                )}
              </Field>
            </Specimen>
            <Specimen
              title="Choices"
              description="Keyboard and pointer controls use the same states."
            >
              <Field label="Default provider">
                {(control) => (
                  <NativeSelect {...control}>
                    <option>Choose a provider</option>
                    <option>Native qualification</option>
                  </NativeSelect>
                )}
              </Field>
              <Field
                label="Unavailable provider"
                error="Connect a provider key before using this source."
              >
                {(control) => (
                  <NativeSelect {...control} disabled>
                    <option>Provider not configured</option>
                  </NativeSelect>
                )}
              </Field>
              <div className="gallery-choice">
                <Checkbox
                  id="gallery-select"
                  checked={checked}
                  onCheckedChange={setChecked}
                />
                <label htmlFor="gallery-select">Select Atlas Systems</label>
              </div>
              <div className="gallery-choice">
                <Checkbox id="gallery-select-disabled" disabled />
                <label htmlFor="gallery-select-disabled">
                  Unavailable selection
                </label>
              </div>
              <div className="gallery-choice">
                <Switch
                  id="gallery-source"
                  checked={enabled}
                  onCheckedChange={setEnabled}
                />
                <label htmlFor="gallery-source">Enable company research</label>
                <StatusBadge tone={enabled ? "positive" : "neutral"}>
                  {enabled ? "Enabled" : "Disabled"}
                </StatusBadge>
              </div>
              <div className="gallery-choice">
                <Switch id="gallery-source-disabled" disabled />
                <label htmlFor="gallery-source-disabled">
                  Source unavailable
                </label>
              </div>
            </Specimen>
            <Specimen
              title="Menus and tabs"
              description="Disclosure, selection, and navigation remain distinct."
            >
              <Menu.Root>
                <Menu.Trigger
                  render={
                    <Button
                      startIcon={
                        <MoreHorizontal aria-hidden="true" className="size-4" />
                      }
                    />
                  }
                >
                  Record actions
                </Menu.Trigger>
                <Menu.Popup>
                  <Menu.Item onClick={() => toast("Preview export requested")}>
                    Export table CSV
                  </Menu.Item>
                  <Menu.Item onClick={() => toast("Preview backup requested")}>
                    Back up records JSON
                  </Menu.Item>
                  <Menu.Item disabled>Restore unavailable</Menu.Item>
                </Menu.Popup>
              </Menu.Root>
              <Tabs
                value={tab}
                onValueChange={(value) => setTab(String(value))}
              >
                <TabsList>
                  <TabsTrigger value="overview">Overview</TabsTrigger>
                  <TabsTrigger value="sources">Sources</TabsTrigger>
                  <TabsTrigger
                    value="activity"
                    disabled
                    aria-label="Activity unavailable in preview"
                  >
                    Activity
                  </TabsTrigger>
                </TabsList>
                <TabsContent value="overview">
                  <p>Accepted company and person facts.</p>
                </TabsContent>
                <TabsContent value="sources">
                  <p>Connected source observations and field ownership.</p>
                </TabsContent>
              </Tabs>
            </Specimen>
            <Specimen
              title="Dialogs and inspectors"
              description="Focus enters, stays contained, and returns when dismissed."
            >
              <div className="gallery-row">
                <Modal open={modal} onOpenChange={setModal}>
                  <ModalTrigger render={<Button />}>Review import</ModalTrigger>
                  <ModalContent>
                    <ModalHeader>
                      <ModalTitle>Review sample import</ModalTitle>
                      <ModalDescription>
                        Two fictional people will be previewed. No records will
                        be created.
                      </ModalDescription>
                    </ModalHeader>
                    <Field
                      label="Receipt email"
                      error={error}
                      description="Enter any complete address to demonstrate validation."
                    >
                      {(control) => (
                        <Input
                          {...control}
                          type="email"
                          value={email}
                          onChange={(event) => {
                            setEmail(event.target.value)
                            setError("")
                          }}
                        />
                      )}
                    </Field>
                    <ModalFooter>
                      <Button onClick={() => setModal(false)}>Cancel</Button>
                      <Button
                        variant="solid"
                        onClick={() => {
                          if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)) {
                            setError("Enter a complete email address.")
                            return
                          }
                          setModal(false)
                          toast.success(
                            "Import preview reviewed. No records created."
                          )
                        }}
                      >
                        Confirm preview
                      </Button>
                    </ModalFooter>
                  </ModalContent>
                </Modal>
                <Sheet>
                  <SheetTrigger render={<Button />}>
                    Open record inspector
                  </SheetTrigger>
                  <SheetContent>
                    <SheetHeader>
                      <SheetTitle>Atlas Systems</SheetTitle>
                      <SheetDescription>
                        Fictional company record
                      </SheetDescription>
                    </SheetHeader>
                    <div className="p-4">
                      <StatusBadge tone="positive">
                        Manually reviewed
                      </StatusBadge>
                      <p className="mt-4 text-sm">
                        Accepted values remain connected across workbooks.
                      </p>
                    </div>
                  </SheetContent>
                </Sheet>
              </div>
            </Specimen>
            <Specimen
              title="Progress and status"
              description="Result labels communicate meaning beyond color."
            >
              <Progress value={70}>
                <ProgressLabel>Enrichment complete</ProgressLabel>
                <ProgressValue />
              </Progress>
              <Progress value={null}>
                <ProgressLabel>Validating import</ProgressLabel>
              </Progress>
              <div className="gallery-row">
                {(
                  [
                    ["neutral", "Queued"],
                    ["accent", "Running"],
                    ["positive", "Complete"],
                    ["warning", "Partial results"],
                    ["danger", "Failed"]
                  ] as const
                ).map(([tone, label]) => (
                  <StatusBadge tone={tone} dot key={tone}>
                    {label}
                  </StatusBadge>
                ))}
              </div>
              <div className="gallery-row">
                <Button onClick={() => toast.success("Shared record saved")}>
                  Show success receipt
                </Button>
                <Button
                  onClick={() =>
                    toast.error("Save failed. Your draft is kept.")
                  }
                >
                  Show error receipt
                </Button>
              </div>
            </Specimen>
            <Specimen
              title="Collection states"
              description="Empty, unavailable, and background work have explicit next steps."
            >
              <ViewState
                title="No matching people"
                description="Clear the current filters to see more records."
                action={<Button>Clear filters</Button>}
              />
              <ViewState
                kind="error"
                title="Records are temporarily unavailable"
                description="Your draft and selection are kept."
                action={<Button>Try again</Button>}
              />
              <ViewState kind="loading" title="Loading companies…" />
            </Specimen>
          </div>
        </main>
        <Toaster />
      </div>
    </TooltipProvider>
  )
}
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider>
      <Gallery />
    </ThemeProvider>
  </StrictMode>
)
