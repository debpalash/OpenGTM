/**
 * Content script — injected into the ACTIVE tab only, on an explicit user
 * click of "Scan page" in the popup (chrome.scripting.executeScript under
 * the activeTab permission). It reads the already-rendered DOM once and
 * reports back; it never navigates, paginates, or fetches anything.
 */

import { extractBest } from "./extract";
import { extractLinkedInPeople } from "./linkedin";
import type { ExtractionMessage } from "./messages";

(() => {
    const host = location.hostname.toLowerCase();
    let extraction = host === "linkedin.com" || host.endsWith(".linkedin.com")
        ? extractLinkedInPeople(document)
        : null;
    if (!extraction || extraction.rows.length === 0) {
        extraction = extractBest(document, location.href);
    }

    const message: ExtractionMessage = {
        type: "YUPCHA_EXTRACTION",
        pageUrl: location.href,
        pageTitle: document.title,
        extraction,
    };
    // Swallow "no receiver" errors (popup closed before the scan finished).
    void chrome.runtime.sendMessage(message).catch(() => undefined);
})();
