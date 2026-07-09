/**
 * Client for the Yupcha inbound rows ingest API:
 *   POST {baseUrl}/api/v2/workbooks/{workbook_id}/rows/ingest
 *
 * Auth: Authorization: Bearer wbi_<token>. One Idempotency-Key per capture
 * batch — we derive a deterministic key per chunk from a UUID minted at
 * capture time, so retries of the same capture never duplicate rows.
 */

export const MAX_ROWS_PER_REQUEST = 500;

export interface IngestResponse {
    workbook_id: string;
    added: number;
    skipped_duplicates: number;
    skipped_empty: number;
    total_rows: number;
    row_ids: string[];
    unmapped_keys: string[];
}

export interface IngestSummary {
    added: number;
    skippedDuplicates: number;
    skippedEmpty: number;
    totalRows: number;
    unmappedKeys: string[];
    chunks: number;
}

export type IngestRow = Record<string, string>;

export function chunkRows<T>(rows: T[], size: number = MAX_ROWS_PER_REQUEST): T[][] {
    const chunks: T[][] = [];
    for (let i = 0; i < rows.length; i += size) chunks.push(rows.slice(i, i + size));
    return chunks;
}

/** Convert an extraction (columns + string matrix) into ingest row objects, dropping empty cells/rows. */
export function buildIngestRows(columns: string[], rows: string[][]): IngestRow[] {
    const out: IngestRow[] = [];
    for (const cells of rows) {
        const row: IngestRow = {};
        columns.forEach((col, i) => {
            const value = cells[i] ?? "";
            if (col && value !== "") row[col] = value;
        });
        if (Object.keys(row).length > 0) out.push(row);
    }
    return out;
}

export class IngestError extends Error {
    constructor(public status: number, message: string) {
        super(message);
        this.name = "IngestError";
    }
}

async function postChunk(
    baseUrl: string,
    workbookId: string,
    token: string,
    rows: IngestRow[],
    idempotencyKey: string,
): Promise<IngestResponse> {
    const res = await fetch(`${baseUrl}/api/v2/workbooks/${encodeURIComponent(workbookId)}/rows/ingest`, {
        method: "POST",
        headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${token}`,
            "Idempotency-Key": idempotencyKey,
        },
        body: JSON.stringify({ rows, dedupe: true }),
    });
    if (!res.ok) {
        const hint =
            res.status === 401 ? "bad or revoked ingest token"
            : res.status === 404 ? "unknown workbook, or inbound rows are disabled on the server"
            : res.status === 413 ? "too many rows in one request"
            : res.status === 422 ? "no valid rows"
            : "server error";
        throw new IngestError(res.status, `Ingest failed (HTTP ${res.status}): ${hint}`);
    }
    return (await res.json()) as IngestResponse;
}

export interface SendOptions {
    baseUrl: string;
    workbookId: string;
    token: string;
    rows: IngestRow[];
    onProgress?: (sentChunks: number, totalChunks: number) => void;
}

/** Chunked send of a capture batch; aggregates per-chunk responses. */
export async function sendRows(opts: SendOptions): Promise<IngestSummary> {
    const chunks = chunkRows(opts.rows);
    const captureId = crypto.randomUUID();
    const summary: IngestSummary = {
        added: 0,
        skippedDuplicates: 0,
        skippedEmpty: 0,
        totalRows: 0,
        unmappedKeys: [],
        chunks: chunks.length,
    };
    for (let i = 0; i < chunks.length; i++) {
        const res = await postChunk(opts.baseUrl, opts.workbookId, opts.token, chunks[i]!, `${captureId}-${i}`);
        summary.added += res.added;
        summary.skippedDuplicates += res.skipped_duplicates;
        summary.skippedEmpty += res.skipped_empty;
        summary.totalRows += res.total_rows;
        for (const key of res.unmapped_keys) {
            if (!summary.unmappedKeys.includes(key)) summary.unmappedKeys.push(key);
        }
        opts.onProgress?.(i + 1, chunks.length);
    }
    return summary;
}
