# @yupcha/chrome-extension — Yupcha Capture

Chrome (Manifest V3) extension that captures structured data from the page you are
currently viewing — HTML tables, repeated list items, and LinkedIn people-search
results — and pushes it into a Yupcha workbook through the inbound rows ingest API
(`POST /api/v2/workbooks/{id}/rows/ingest`).

It only ever reads the **currently open page, on an explicit click**. No background
scraping, no pagination walking, no `<all_urls>` API access.

## Build & load

```sh
bun install
bun run build        # produces dist/ (manifest + bundles)
bun run test         # extraction-heuristics unit tests
bun run zip          # optional: yupcha-chrome-extension.zip
```

1. Open `chrome://extensions`, enable **Developer mode**, click **Load unpacked**,
   and pick this package's `dist/` folder.
2. In Yupcha, open the workbook you want to feed and create an **ingest token**
   (`wbi_…`).
3. In the extension's **Options** page: enter your Yupcha base URL
   (e.g. `https://yupcha.example.com`), add the workbook ID + `wbi_` token, and
   **Save**. Chrome will ask to grant access to that origin only.

## Use

Open any page with a table / repeated list (or LinkedIn people search), click the
extension icon → **Scan page**. Review the preview (rename or exclude columns —
names are matched case-insensitively to your workbook's columns, e.g. "Work Email"),
pick the target workbook, then **Send N rows**. Sends are chunked at 500 rows per
request with an idempotency key per capture, so retries never duplicate rows.

## Permissions model

- `activeTab` + `scripting`: inject the extractor into the active tab on click.
- `storage`: settings (base URL, workbook tokens) in `chrome.storage.local`.
- API host access is **runtime-only** (`optional_host_permissions`), requested for
  exactly the base URL origin you enter in Options.

## Notes

- LinkedIn selectors live in `src/linkedin.ts` only and are best-effort — LinkedIn's
  markup changes often; the generic table/list heuristics are the fallback.
- Ingest tokens are stored in `chrome.storage.local` on your machine; treat your
  Chrome profile accordingly, and revoke tokens in Yupcha if in doubt.
