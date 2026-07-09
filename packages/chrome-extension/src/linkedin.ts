/**
 * LinkedIn people-search extraction.
 *
 * WARNING — SELECTOR ROT: LinkedIn ships obfuscated, frequently-changing
 * markup. Everything site-specific lives in THIS module only; each field is
 * tried against a list of best-effort selectors (newest first) and the
 * extractor degrades gracefully to the generic heuristics in extract.ts
 * when nothing matches. Expect to update the selector lists over time.
 *
 * The extension only reads the page the user is currently viewing, on an
 * explicit click — it never paginates or crawls.
 */

import type { Extraction } from "./extract";
import { classifyHref, normalizeText } from "./extract";

/** Containers that hold one person result each (newest variants first). */
const RESULT_ITEM_SELECTORS = [
    'div[data-view-name="search-entity-result-universal-template"]',
    "li.reusable-search__result-container",
    "div.entity-result",
    "li.search-result__occluded-item",
];

const TITLE_SELECTORS = [
    ".entity-result__primary-subtitle",
    'div[data-view-name="search-entity-result-universal-template"] .t-14.t-black.t-normal',
    ".t-14.t-black.t-normal",
    ".subline-level-1",
];

const LOCATION_SELECTORS = [
    ".entity-result__secondary-subtitle",
    ".t-14.t-normal:not(.t-black)",
    ".subline-level-2",
];

function firstText(item: Element, selectors: string[]): string {
    for (const sel of selectors) {
        const el = item.querySelector(sel);
        const text = normalizeText(el?.textContent ?? "");
        if (text) return text;
    }
    return "";
}

function profileLink(item: Element): { name: string; url: string } | null {
    for (const a of Array.from(item.querySelectorAll("a[href]"))) {
        const classified = classifyHref(a.getAttribute("href") ?? "", "https://www.linkedin.com/");
        if (!classified || classified.column !== "linkedin_url") continue;
        // Visible name is usually inside an aria-hidden span (its sibling is
        // a visually-hidden "View X's profile" copy we want to avoid).
        const ariaHidden = a.querySelector('span[aria-hidden="true"]');
        let name = normalizeText(ariaHidden?.textContent ?? "");
        if (!name) {
            name = normalizeText(a.textContent ?? "").replace(/View .+?['’]s profile.*$/i, "").trim();
        }
        if (name) return { name, url: classified.value };
    }
    return null;
}

/**
 * Extract LinkedIn people-search results into
 * full_name / title / location / linkedin_url columns.
 * Returns null when the page doesn't look like people-search results.
 */
export function extractLinkedInPeople(doc: Document): Extraction | null {
    let items: Element[] = [];
    for (const sel of RESULT_ITEM_SELECTORS) {
        items = Array.from(doc.querySelectorAll(sel));
        if (items.length > 0) break;
    }
    if (items.length === 0) return null;

    const rows: string[][] = [];
    for (const item of items) {
        const link = profileLink(item);
        if (!link) continue;
        rows.push([link.name, firstText(item, TITLE_SELECTORS), firstText(item, LOCATION_SELECTORS), link.url]);
    }

    if (rows.length === 0) return null;
    return { source: "linkedin", columns: ["full_name", "title", "location", "linkedin_url"], rows };
}
