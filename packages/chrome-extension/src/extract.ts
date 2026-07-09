/**
 * Pure, DOM-in / data-out extraction heuristics.
 *
 * No chrome.* APIs, no fetch, no globals beyond the Document/Element passed
 * in — this module is unit-tested with happy-dom (see test/extract.test.ts).
 */

export type ExtractSource = "table" | "list" | "linkedin";

export interface Extraction {
    source: ExtractSource;
    /** Detected column names, aligned with each row's cells. */
    columns: string[];
    /** Row-major cell text; every row has exactly columns.length entries ("" for missing). */
    rows: string[][];
}

export type HrefColumn = "email" | "linkedin_url" | "website";

export interface ClassifiedHref {
    column: HrefColumn;
    value: string;
}

/** Collapse whitespace runs and trim. */
export function normalizeText(text: string): string {
    return text.replace(/\s+/g, " ").trim();
}

/**
 * Classify a raw href into an ingest-friendly column.
 *   mailto:            -> email
 *   linkedin.com/in/…  -> linkedin_url (query/fragment stripped)
 *   other http(s)      -> website
 *   anything else      -> null (fragments, javascript:, tel:, unresolvable relative)
 */
export function classifyHref(href: string, baseUrl?: string): ClassifiedHref | null {
    const raw = href.trim();
    if (!raw || raw.startsWith("#") || /^(javascript|data|blob):/i.test(raw)) return null;

    if (/^mailto:/i.test(raw)) {
        const email = raw.slice("mailto:".length).split("?")[0]?.trim() ?? "";
        return email.includes("@") ? { column: "email", value: email } : null;
    }

    let url: URL;
    try {
        url = baseUrl ? new URL(raw, baseUrl) : new URL(raw);
    } catch {
        return null;
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") return null;

    const host = url.hostname.toLowerCase().replace(/^www\./, "");
    if ((host === "linkedin.com" || host.endsWith(".linkedin.com")) && /^\/in\/[^/]/.test(url.pathname)) {
        // Strip tracking query params / fragments from profile URLs.
        return { column: "linkedin_url", value: url.origin + url.pathname.replace(/\/$/, "") };
    }
    return { column: "website", value: url.href };
}

// ---------------------------------------------------------------------------
// <table> extraction
// ---------------------------------------------------------------------------

function ownRows(table: Element): Element[] {
    return Array.from(table.querySelectorAll("tr")).filter((tr) => tr.closest("table") === table);
}

function cellTexts(row: Element): string[] {
    return Array.from(row.querySelectorAll("th, td"))
        .filter((cell) => cell.closest("tr") === row)
        .map((cell) => normalizeText(cell.textContent ?? ""));
}

/**
 * Extract every non-nested <table>: header cells become column names
 * (synthesized as column_N when the table has no header row), remaining
 * rows become data rows.
 */
export function extractTables(doc: Document): Extraction[] {
    const out: Extraction[] = [];
    for (const table of Array.from(doc.querySelectorAll("table"))) {
        if (table.querySelector("table")) continue; // layout wrapper — inner tables are visited on their own

        const rows = ownRows(table);
        if (rows.length === 0) continue;

        const firstRow = rows[0]!;
        const firstCells = Array.from(firstRow.querySelectorAll("th, td")).filter((c) => c.closest("tr") === firstRow);
        const hasHeader =
            firstRow.closest("thead") !== null ||
            (firstCells.length > 0 && firstCells.every((c) => c.tagName.toLowerCase() === "th"));

        let columns: string[];
        let dataRows: Element[];
        if (hasHeader) {
            columns = cellTexts(firstRow).map((t, i) => t || `column_${i + 1}`);
            dataRows = rows.slice(1);
        } else {
            columns = cellTexts(firstRow).map((_, i) => `column_${i + 1}`);
            dataRows = rows;
        }
        if (columns.length === 0) continue;

        const data = dataRows
            .map((row) => {
                const cells = cellTexts(row);
                return columns.map((_, i) => cells[i] ?? "");
            })
            .filter((cells) => cells.some((c) => c !== ""));

        if (data.length === 0) continue;
        out.push({ source: "table", columns, rows: data });
    }
    return out;
}

// ---------------------------------------------------------------------------
// Repeated-structure list extraction
// ---------------------------------------------------------------------------

const SKIP_TAGS = new Set([
    "script", "style", "noscript", "template", "svg", "path", "iframe",
    "tr", "td", "th", "thead", "tbody", "option", "br", "hr", "head", "meta", "link",
]);

function signature(el: Element): string {
    const classes = Array.from(el.classList).sort().join(".");
    return `${el.tagName.toLowerCase()}|${classes}`;
}

/** Depth-first list of leaf elements that carry visible text. */
function leafTexts(el: Element, cap = 12): string[] {
    const out: string[] = [];
    const visit = (node: Element): void => {
        if (out.length >= cap) return;
        const tag = node.tagName.toLowerCase();
        if (SKIP_TAGS.has(tag)) return;
        const children = Array.from(node.children).filter((c) => !SKIP_TAGS.has(c.tagName.toLowerCase()));
        if (children.length === 0) {
            const text = normalizeText(node.textContent ?? "");
            if (text && text !== out[out.length - 1]) out.push(text);
            return;
        }
        for (const child of children) visit(child);
    };
    visit(el);
    return out;
}

/** First classified href of each kind found inside an item. */
function itemHrefs(el: Element, baseUrl?: string): Partial<Record<HrefColumn, string>> {
    const found: Partial<Record<HrefColumn, string>> = {};
    for (const a of Array.from(el.querySelectorAll("a[href]"))) {
        const classified = classifyHref(a.getAttribute("href") ?? "", baseUrl);
        if (classified && !(classified.column in found)) found[classified.column] = classified.value;
    }
    return found;
}

/**
 * Find the largest group of sibling elements with the same tag + class
 * signature ("repeated structure"), then flatten each item's leaf text
 * nodes into text_N columns and its hrefs into email / linkedin_url /
 * website columns.
 */
export function extractRepeatedList(doc: Document, baseUrl?: string): Extraction | null {
    interface Group { items: Element[]; score: number; }
    let best: Group | null = null;

    const parents = new Set<Element>();
    for (const el of Array.from(doc.querySelectorAll("*"))) {
        const parent = el.parentElement;
        if (parent) parents.add(parent);
    }

    for (const parent of parents) {
        if (parent.closest("table")) continue;
        const bySig = new Map<string, Element[]>();
        for (const child of Array.from(parent.children)) {
            const tag = child.tagName.toLowerCase();
            if (SKIP_TAGS.has(tag)) continue;
            const sig = signature(child);
            const arr = bySig.get(sig) ?? [];
            arr.push(child);
            bySig.set(sig, arr);
        }
        for (const items of bySig.values()) {
            if (items.length < 3) continue;
            const sampled = items.slice(0, 5);
            const avgLeaves =
                sampled.reduce((sum, item) => sum + leafTexts(item, 6).length, 0) / sampled.length;
            if (avgLeaves < 1) continue; // e.g. icon-only nav items
            const score = items.length * (1 + avgLeaves);
            if (!best || score > best.score) best = { items, score };
        }
    }

    if (!best) return null;

    const perItemTexts = best.items.map((item) => leafTexts(item));
    const perItemHrefs = best.items.map((item) => itemHrefs(item, baseUrl));

    // Align text columns on the most common leaf count (mode), capped at 8.
    const countFreq = new Map<number, number>();
    for (const texts of perItemTexts) {
        const n = Math.min(texts.length, 8);
        countFreq.set(n, (countFreq.get(n) ?? 0) + 1);
    }
    let textCols = 0;
    let bestFreq = 0;
    for (const [n, freq] of countFreq) {
        if (freq > bestFreq || (freq === bestFreq && n > textCols)) {
            textCols = n;
            bestFreq = freq;
        }
    }

    const hrefCols: HrefColumn[] = (["email", "linkedin_url", "website"] as const).filter((col) =>
        perItemHrefs.some((h) => h[col] !== undefined),
    );

    const columns = [
        ...Array.from({ length: textCols }, (_, i) => `text_${i + 1}`),
        ...hrefCols,
    ];
    if (columns.length === 0) return null;

    const rows = best.items
        .map((_, idx) => {
            const texts = perItemTexts[idx]!;
            const hrefs = perItemHrefs[idx]!;
            return [
                ...Array.from({ length: textCols }, (_, i) => texts[i] ?? ""),
                ...hrefCols.map((col) => hrefs[col] ?? ""),
            ];
        })
        .filter((cells) => cells.some((c) => c !== ""));

    if (rows.length < 3) return null;
    return { source: "list", columns, rows };
}

// ---------------------------------------------------------------------------
// Best-candidate selection
// ---------------------------------------------------------------------------

function score(e: Extraction): number {
    // Tables are already-structured data; weight them above heuristic lists.
    const bias = e.source === "table" ? 2 : 1;
    return bias * e.rows.length * e.columns.length;
}

/**
 * Best generic extraction for a page: the highest-scoring candidate among
 * all tables and the largest repeated list. Returns null on pages with no
 * structured data.
 */
export function extractBest(doc: Document, baseUrl?: string): Extraction | null {
    const candidates: Extraction[] = [...extractTables(doc)];
    const list = extractRepeatedList(doc, baseUrl);
    if (list) candidates.push(list);
    if (candidates.length === 0) return null;
    return candidates.reduce((a, b) => (score(b) > score(a) ? b : a));
}
