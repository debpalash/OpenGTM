---
title: Chrome capture extension
description: Capture tables, repeated lists and LinkedIn people-search results from the page you are viewing into a workbook.
sidebar:
  order: 3
---

`packages/chrome-extension` is a Manifest V3 extension that captures structured
data from the page you are currently viewing (HTML tables, repeated list items,
and LinkedIn people-search results) and pushes it into an OpenGTM workbook
through the inbound rows ingest API,
`POST /api/v2/workbooks/{id}/rows/ingest`.

It only reads the **currently open page, on an explicit click**. There is no
background scraping, no pagination walking, and no `<all_urls>` permission.

## Build and load

```bash
cd packages/chrome-extension
bun install
bun run build   # produces dist/ (manifest + bundles)
bun run test    # extraction-heuristics unit tests
bun run zip     # optional: opengtm-chrome-extension.zip
```

1. Open `chrome://extensions`, enable **Developer mode**, click **Load
   unpacked**, and pick the package's `dist/` folder.
2. In OpenGTM, open the workbook you want to feed and create an **ingest
   token** (they start with `wbi_`). See [Ingest API](/guides/ingest-api/).
3. In the extension's **Options** page enter your OpenGTM base URL (for
   example `https://opengtm.example.com`), add the workbook ID and the `wbi_`
   token, and **Save**. Chrome asks to grant access to that origin only.

## Use

Open any page with a table or repeated list (or a LinkedIn people search),
click the extension icon, then **Scan page**. Review the preview, rename or
exclude columns (names are matched case-insensitively to your workbook's
columns, so "Work Email" lands in the `Work Email` column), pick the target
workbook, then **Send N rows**.

Sends are chunked at 500 rows per request with an idempotency key per capture,
so retries never duplicate rows.

## Permissions model

- `activeTab` + `scripting`: inject the extractor into the active tab on click.
- `storage`: settings (base URL, workbook tokens) in `chrome.storage.local`.
- API host access is **runtime-only** (`optional_host_permissions`), requested
  for exactly the base-URL origin you entered.

## Notes

- LinkedIn selectors live in `src/linkedin.ts` and are best-effort; LinkedIn's
  markup changes often, and the generic table/list heuristics are the fallback.
- Ingest tokens are stored on your machine in `chrome.storage.local`. Treat
  your Chrome profile accordingly and revoke tokens in OpenGTM if in doubt.
