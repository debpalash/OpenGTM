/**
 * Options page: Yupcha base URL + per-workbook ingest tokens.
 * Saving requests a runtime host permission for the base URL origin only
 * (the manifest ships no API host_permissions at all).
 */

import { loadSettings, normalizeBaseUrl, saveSettings, type WorkbookEntry } from "./storage";

function el<T extends HTMLElement>(id: string): T {
    const node = document.getElementById(id);
    if (!node) throw new Error(`missing #${id}`);
    return node as T;
}

function setStatus(text: string, kind: "info" | "error" | "ok" = "info"): void {
    const status = el<HTMLDivElement>("status");
    status.textContent = text;
    status.dataset["kind"] = kind;
}

function workbookRow(entry?: Partial<WorkbookEntry>): HTMLTableRowElement {
    const tr = document.createElement("tr");
    tr.className = "workbook-row";
    const mk = (cls: string, placeholder: string, value: string, type = "text"): HTMLInputElement => {
        const input = document.createElement("input");
        input.type = type;
        input.className = cls;
        input.placeholder = placeholder;
        input.value = value;
        return input;
    };
    const cells: HTMLElement[] = [
        mk("wb-label", "Label (optional)", entry?.label ?? ""),
        mk("wb-id", "Workbook ID (UUID)", entry?.id ?? ""),
        mk("wb-token", "wbi_…", entry?.token ?? "", "password"),
    ].map((input) => {
        const td = document.createElement("td");
        td.appendChild(input);
        return td;
    });
    const removeTd = document.createElement("td");
    const remove = document.createElement("button");
    remove.type = "button";
    remove.textContent = "Remove";
    remove.addEventListener("click", () => tr.remove());
    removeTd.appendChild(remove);
    tr.append(...cells, removeTd);
    return tr;
}

function readWorkbooks(): WorkbookEntry[] {
    const rows = Array.from(document.querySelectorAll<HTMLTableRowElement>("tr.workbook-row"));
    const out: WorkbookEntry[] = [];
    for (const row of rows) {
        const label = row.querySelector<HTMLInputElement>(".wb-label")?.value.trim() ?? "";
        const id = row.querySelector<HTMLInputElement>(".wb-id")?.value.trim() ?? "";
        const token = row.querySelector<HTMLInputElement>(".wb-token")?.value.trim() ?? "";
        if (!id && !token) continue; // blank row
        if (!id || !token) throw new Error("Each workbook needs both an ID and a wbi_ token.");
        if (!token.startsWith("wbi_")) throw new Error(`Ingest tokens start with wbi_ (workbook ${id}).`);
        out.push({ id, label, token });
    }
    return out;
}

async function save(): Promise<void> {
    try {
        const baseUrl = normalizeBaseUrl(el<HTMLInputElement>("base-url").value);
        if (!baseUrl) throw new Error("Enter a valid http(s) Yupcha base URL.");
        const workbooks = readWorkbooks();

        // Runtime-only host permission for exactly the user's Yupcha origin.
        const origin = `${baseUrl}/*`;
        const granted = await chrome.permissions.request({ origins: [origin] });
        if (!granted) {
            setStatus(`Permission for ${origin} was declined — sending to Yupcha will fail until granted.`, "error");
        }

        await saveSettings({ baseUrl, workbooks });
        el<HTMLInputElement>("base-url").value = baseUrl;
        setStatus(
            granted
                ? `Saved. ${workbooks.length} workbook(s) configured; access to ${origin} granted.`
                : `Saved ${workbooks.length} workbook(s), but host permission is missing.`,
            granted ? "ok" : "error",
        );
    } catch (err) {
        setStatus(err instanceof Error ? err.message : String(err), "error");
    }
}

async function init(): Promise<void> {
    const settings = await loadSettings();
    el<HTMLInputElement>("base-url").value = settings.baseUrl;
    const tbody = el<HTMLTableSectionElement>("workbooks");
    for (const wb of settings.workbooks) tbody.appendChild(workbookRow(wb));
    if (settings.workbooks.length === 0) tbody.appendChild(workbookRow());

    el<HTMLButtonElement>("add-workbook").addEventListener("click", () => tbody.appendChild(workbookRow()));
    el<HTMLButtonElement>("save").addEventListener("click", () => void save());
}

void init();
