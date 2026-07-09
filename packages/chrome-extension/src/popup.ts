/**
 * Popup: Scan page -> preview extracted rows -> rename/exclude columns ->
 * pick workbook -> chunked send with progress + result summary.
 */

import { buildIngestRows, sendRows } from "./api";
import type { Extraction } from "./extract";
import { isExtractionMessage } from "./messages";
import { loadSettings, type Settings } from "./storage";

const PREVIEW_ROWS = 10;
const SCAN_TIMEOUT_MS = 8000;

interface ColumnState {
    name: string;
    included: boolean;
}

let settings: Settings = { baseUrl: "", workbooks: [] };
let extraction: Extraction | null = null;
let columnState: ColumnState[] = [];

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

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

function waitForExtraction(): Promise<Extraction | null> {
    return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
            chrome.runtime.onMessage.removeListener(listener);
            reject(new Error("Timed out waiting for the page scan."));
        }, SCAN_TIMEOUT_MS);
        const listener = (msg: unknown): void => {
            if (!isExtractionMessage(msg)) return;
            clearTimeout(timer);
            chrome.runtime.onMessage.removeListener(listener);
            resolve(msg.extraction);
        };
        chrome.runtime.onMessage.addListener(listener);
    });
}

async function scanPage(): Promise<void> {
    setStatus("Scanning page…");
    el<HTMLElement>("results").hidden = true;
    try {
        const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
        if (!tab?.id) throw new Error("No active tab.");
        if (!/^https?:/.test(tab.url ?? "")) throw new Error("This page cannot be scanned (not http/https).");

        // Register the listener BEFORE injecting so the result cannot be missed.
        const pending = waitForExtraction();
        await chrome.scripting.executeScript({ target: { tabId: tab.id }, files: ["content.js"] });
        extraction = await pending;

        if (!extraction || extraction.rows.length === 0) {
            setStatus("No structured data found on this page (no tables, repeated lists, or LinkedIn results).", "error");
            return;
        }
        columnState = extraction.columns.map((name) => ({ name, included: true }));
        renderResults();
        const label =
            extraction.source === "table" ? "HTML table"
            : extraction.source === "linkedin" ? "LinkedIn people results"
            : "repeated list";
        setStatus(`Found ${extraction.rows.length} rows (${label}).`, "ok");
    } catch (err) {
        setStatus(err instanceof Error ? err.message : String(err), "error");
    }
}

// ---------------------------------------------------------------------------
// Preview rendering
// ---------------------------------------------------------------------------

function renderResults(): void {
    if (!extraction) return;
    const results = el<HTMLElement>("results");
    results.hidden = false;

    const head = el<HTMLTableRowElement>("preview-head");
    head.replaceChildren();
    columnState.forEach((col, i) => {
        const th = document.createElement("th");
        const include = document.createElement("input");
        include.type = "checkbox";
        include.checked = col.included;
        include.title = "Include this column";
        include.addEventListener("change", () => {
            col.included = include.checked;
            updateSendButton();
        });
        const name = document.createElement("input");
        name.type = "text";
        name.value = col.name;
        name.title = "Column name sent to Yupcha (matched to workbook columns case-insensitively)";
        name.addEventListener("input", () => {
            columnState[i]!.name = name.value.trim();
        });
        th.append(include, name);
        head.appendChild(th);
    });

    const body = el<HTMLTableSectionElement>("preview-body");
    body.replaceChildren();
    for (const row of extraction.rows.slice(0, PREVIEW_ROWS)) {
        const tr = document.createElement("tr");
        for (const cell of row) {
            const td = document.createElement("td");
            td.textContent = cell.length > 80 ? `${cell.slice(0, 80)}…` : cell;
            td.title = cell;
            tr.appendChild(td);
        }
        body.appendChild(tr);
    }
    el<HTMLDivElement>("preview-note").textContent =
        extraction.rows.length > PREVIEW_ROWS
            ? `Showing first ${PREVIEW_ROWS} of ${extraction.rows.length} rows.`
            : `${extraction.rows.length} rows.`;

    renderWorkbookSelect();
    updateSendButton();
}

function renderWorkbookSelect(): void {
    const select = el<HTMLSelectElement>("workbook");
    select.replaceChildren();
    for (const wb of settings.workbooks) {
        const opt = document.createElement("option");
        opt.value = wb.id;
        opt.textContent = wb.label ? `${wb.label} (${wb.id.slice(0, 8)}…)` : wb.id;
        select.appendChild(opt);
    }
}

function updateSendButton(): void {
    const send = el<HTMLButtonElement>("send");
    const rowCount = extraction?.rows.length ?? 0;
    const anyColumn = columnState.some((c) => c.included && c.name !== "");
    send.disabled = rowCount === 0 || !anyColumn || settings.workbooks.length === 0;
    send.textContent = `Send ${rowCount} rows`;
}

// ---------------------------------------------------------------------------
// Send
// ---------------------------------------------------------------------------

async function send(): Promise<void> {
    if (!extraction) return;
    const workbookId = el<HTMLSelectElement>("workbook").value;
    const workbook = settings.workbooks.find((w) => w.id === workbookId);
    if (!workbook) {
        setStatus("Pick a workbook first (add one in Options).", "error");
        return;
    }

    const includedIdx = columnState.map((c, i) => (c.included && c.name ? i : -1)).filter((i) => i >= 0);
    const columns = includedIdx.map((i) => columnState[i]!.name);
    const rows = buildIngestRows(columns, extraction.rows.map((r) => includedIdx.map((i) => r[i] ?? "")));
    if (rows.length === 0) {
        setStatus("Nothing to send — all rows are empty after column selection.", "error");
        return;
    }

    const sendBtn = el<HTMLButtonElement>("send");
    sendBtn.disabled = true;
    try {
        const summary = await sendRows({
            baseUrl: settings.baseUrl,
            workbookId: workbook.id,
            token: workbook.token,
            rows,
            onProgress: (done, total) => setStatus(`Sending… chunk ${done}/${total}`),
        });
        const parts = [`Added ${summary.added}`];
        if (summary.skippedDuplicates) parts.push(`${summary.skippedDuplicates} duplicates skipped`);
        if (summary.skippedEmpty) parts.push(`${summary.skippedEmpty} empty skipped`);
        if (summary.unmappedKeys.length) parts.push(`unmapped columns kept verbatim: ${summary.unmappedKeys.join(", ")}`);
        setStatus(`${parts.join(" · ")} (of ${summary.totalRows} sent).`, "ok");
    } catch (err) {
        setStatus(err instanceof Error ? err.message : String(err), "error");
    } finally {
        sendBtn.disabled = false;
    }
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

async function init(): Promise<void> {
    settings = await loadSettings();
    if (!settings.baseUrl || settings.workbooks.length === 0) {
        setStatus("Set your Yupcha URL and a workbook ingest token in Options first.", "error");
    }
    el<HTMLButtonElement>("scan").addEventListener("click", () => void scanPage());
    el<HTMLButtonElement>("send").addEventListener("click", () => void send());
    el<HTMLAnchorElement>("open-options").addEventListener("click", (e) => {
        e.preventDefault();
        void chrome.runtime.openOptionsPage();
    });
}

void init();
