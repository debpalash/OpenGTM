import type { Extraction } from "./extract";

/** Message the injected content script sends back to the popup. */
export interface ExtractionMessage {
    type: "YUPCHA_EXTRACTION";
    pageUrl: string;
    pageTitle: string;
    extraction: Extraction | null;
}

export function isExtractionMessage(msg: unknown): msg is ExtractionMessage {
    return typeof msg === "object" && msg !== null && (msg as { type?: unknown }).type === "YUPCHA_EXTRACTION";
}
