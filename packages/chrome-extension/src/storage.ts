/** Settings persisted in chrome.storage.local. */

export interface WorkbookEntry {
    /** OpenGTM workbook UUID. */
    id: string;
    /** Optional human label shown in the popup dropdown. */
    label: string;
    /** Per-workbook ingest token (wbi_…). */
    token: string;
}

export interface Settings {
    /** OpenGTM base URL, e.g. https://opengtm.example.com (no trailing slash). */
    baseUrl: string;
    workbooks: WorkbookEntry[];
}

const DEFAULTS: Settings = { baseUrl: "", workbooks: [] };

export async function loadSettings(): Promise<Settings> {
    const stored = await chrome.storage.local.get("settings");
    const s = stored["settings"] as Partial<Settings> | undefined;
    return {
        baseUrl: typeof s?.baseUrl === "string" ? s.baseUrl : DEFAULTS.baseUrl,
        workbooks: Array.isArray(s?.workbooks) ? (s.workbooks as WorkbookEntry[]) : DEFAULTS.workbooks,
    };
}

export async function saveSettings(settings: Settings): Promise<void> {
    await chrome.storage.local.set({ settings });
}

/** Normalize a user-entered base URL: require http(s), strip trailing slash and path. */
export function normalizeBaseUrl(input: string): string | null {
    const raw = input.trim();
    if (!raw) return null;
    let url: URL;
    try {
        url = new URL(raw.includes("://") ? raw : `https://${raw}`);
    } catch {
        return null;
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") return null;
    return url.origin;
}
