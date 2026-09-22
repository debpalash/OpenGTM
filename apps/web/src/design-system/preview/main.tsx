import { StrictMode, useState } from "react"
import { createRoot } from "react-dom/client"
import { HashRouter } from "react-router-dom"
import { UndecoratedLink } from "twenty-ui/primitives/navigation"
import { ThemeProvider } from "../theme/theme-provider"
import { useTheme } from "../theme/use-theme"
import { ThemeSelect } from "../theme/theme-select"
import { Button, Input, AlertDialog, Menu } from "../primitives"
import "@fontsource/inter/latin-400.css"
import "@fontsource/inter/latin-500.css"
import "@fontsource/inter/latin-600.css"
import "twenty-ui/theme-light.css"
import "twenty-ui/theme-dark.css"
import "./preview.css"

const records = [
  { id: "sample-1", name: "Alex Morgan", company: "Example Labs", title: "Head of Partnerships", state: "Needs verification" },
  { id: "sample-2", name: "Taylor Reed", company: "Example Labs", title: "Partner Operations", state: "Source unavailable" },
  { id: "sample-3", name: "Jordan Lee", company: "Example Systems", title: "Strategic Alliances", state: "Not researched" },
]

function Preview() {
  const { colorScheme: scheme, setPreference: setScheme } = useTheme()
  const [search, setSearch] = useState("")
  const [selected, setSelected] = useState<string[]>([])
  const [comfortable, setComfortable] = useState(false)
  const [receipt, setReceipt] = useState("")
  const visible = records.filter(record => `${record.name} ${record.company}`.toLowerCase().includes(search.toLowerCase()))

  return <>
    <div className="ui-preview">
      <header className="preview-header">
        <a className="preview-brand" href="/">OpenGTM</a>
        <span className="preview-note">Design-system preview · fictional data</span>
        <ThemeSelect />
        <Button onClick={() => setScheme(scheme === "light" ? "dark" : "light")}>
          {scheme === "light" ? "Dark theme" : "Light theme"}
        </Button>
      </header>
      <main>
        <div className="preview-heading">
          <div><p className="preview-eyebrow">Workbooks</p><h1>Partnership research</h1></div>
          <AlertDialog.Root>
            <AlertDialog.Trigger render={<Button disabled={!selected.length} />}>
              Review selection
            </AlertDialog.Trigger>
            <AlertDialog.Popup>
              <AlertDialog.Title>Review {selected.length} selected contacts</AlertDialog.Title>
              <AlertDialog.Description>
                This preview does not contact providers, spend credits, or save leads. Selection includes hidden filtered rows.
              </AlertDialog.Description>
              <ul>{records.filter(record => selected.includes(record.id)).map(record => <li key={record.id}>{record.name}</li>)}</ul>
              <div className="preview-actions">
                <AlertDialog.Close render={<Button />}>Cancel</AlertDialog.Close>
                <AlertDialog.Close render={<Button />} onClick={() => setReceipt(`Preview reviewed: ${selected.length} contacts. No actions executed.`)}>
                  Confirm preview
                </AlertDialog.Close>
              </div>
            </AlertDialog.Popup>
          </AlertDialog.Root>
        </div>
        <section className="preview-records" aria-label="Research contacts">
          <div className="preview-toolbar">
            <label className="preview-search">Search contacts<Input value={search} onChange={event => setSearch(event.target.value)} placeholder="Name or company" /></label>
            <span role="status">{selected.length} selected · {visible.length} shown</span>
            <Menu.Root>
              <Menu.Trigger render={<Button />}>View options</Menu.Trigger>
              <Menu.Popup>
                <Menu.CheckboxItem checked={comfortable} onCheckedChange={setComfortable}>Comfortable rows</Menu.CheckboxItem>
                <Menu.Item onClick={() => setSelected([])} disabled={!selected.length}>Clear selection</Menu.Item>
              </Menu.Popup>
            </Menu.Root>
          </div>
          <div className="preview-table-scroll" role="region" aria-label="Contacts table" tabIndex={0}>
            <table data-comfortable={comfortable}>
              <thead><tr><th scope="col">Select</th><th scope="col">Person</th><th scope="col">Company</th><th scope="col">Research status</th></tr></thead>
              <tbody>{visible.map(record => <tr key={record.id} data-selected={selected.includes(record.id)}>
                <td><input type="checkbox" aria-label={`Select ${record.name}`} checked={selected.includes(record.id)} onChange={event => setSelected(previous => event.target.checked ? [...previous, record.id] : previous.filter(id => id !== record.id))} /></td>
                <td><span className="preview-person">{record.name}</span><span className="preview-secondary">{record.title}</span></td>
                <td>{record.company}</td><td><span className="preview-status">{record.state}</span></td>
              </tr>)}</tbody>
            </table>
          </div>
          {!visible.length && <p className="preview-empty">No contacts match. Try another name or company.</p>}
          <footer className="preview-table-footer">Sample records only. No employment or contact information has been verified.</footer>
        </section>
        <p role="status">{receipt}</p>
        <section className="preview-controls" aria-label="Component states">
          <h2>Action states</h2>
          <div className="preview-actions"><Button disabled>Unavailable</Button><Button loading>Researching</Button><UndecoratedLink to="/workbooks">Test workbook navigation</UndecoratedLink></div>
          <p className="preview-secondary">No backend requests are made from this page.</p>
        </section>
      </main>
    </div>
  </>
}

createRoot(document.getElementById("root")!).render(<StrictMode><ThemeProvider><HashRouter><Preview /></HashRouter></ThemeProvider></StrictMode>)
