# Web benchmark results: web-baseline

- Created: 2026-10-05T05:52:00+00:00 (took 646.3 s)
- Commit: `e8f5568f6b44` on `rewrite/web-and-fastapi-wedge` (working tree dirty)
- Machine: AMD Ryzen 9 7950X 16-Core Processor, 32 logical cores, 61.9 GB RAM, Omarchy / 7.2.5-3-omarchy
- Load average (1/5/15 min) at start: 18.0, 14.9, 14.0; at end: 18.0, 14.9, 14.0
- Browser: 153.0.8010.12; Playwright 1.63.0; bun 1.4.2
- 5 repetition(s) per point, fresh browser context (cold cache) each; cells are the median with the min-max range. Backend API answered from fixtures; see the README for what that does and does not cover.

## Bundle size (gzip, deterministic)

Initial JS (entry + static imports): **223.33 kB** in 50 chunks (666.88 kB raw); initial CSS 32.69 kB. All JS: 786.41 kB in 163 chunks; all CSS 43.48 kB.

| route (page chunk) | route JS kB gzip | route CSS kB | first visit JS kB (initial + route) |
|---|---:|---:|---:|
| agents | 33.58 | 0.0 | 256.91 |
| analytics | 121.09 | 0.0 | 344.43 |
| audiences | 9.14 | 0.0 | 232.47 |
| automations | 37.92 | 0.0 | 261.26 |
| campaigns | 18.79 | 0.0 | 242.12 |
| chat | 236.65 | 1.75 | 459.98 |
| lead-detail | 122.61 | 6.37 | 345.94 |
| leads | 46.5 | 0.0 | 269.83 |
| login | 10.24 | 1.26 | 233.58 |
| notifications | 2.18 | 0.0 | 225.51 |
| outreach | 42.04 | 0.0 | 265.37 |
| plugin-run | 39.17 | 0.0 | 262.5 |
| plugins | 50.95 | 0.0 | 274.28 |
| search | 7.8 | 0.0 | 231.13 |
| settings | 24.61 | 0.0 | 247.94 |
| signals | 126.26 | 0.0 | 349.59 |
| sources | 10.6 | 0.0 | 233.93 |
| task-detail | 22.42 | 0.0 | 245.75 |
| templates | 6.39 | 0.0 | 229.72 |
| watches | 30.16 | 0.0 | 253.49 |
| workbook-editor | 80.6 | 0.88 | 303.93 |
| workbooks | 9.02 | 0.91 | 232.35 |
| workspaces-manager | 3.02 | 0.0 | 226.36 |

Route JS is what the page needs on top of the initial bundle; chunks shared by several pages are counted for each.

## Cold load, `desktop` profile (no throttling)

| route | TTFB ms | FCP ms | LCP ms | heading ready ms | load ms | CLS | TBT ms | JS transferred kB | requests |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| chat | 1.8 | 808 (784-824) | 808 (784-824) | 786.5 (766.5-798.8) | 375.1 | 0 | 0 | 461.63 | 114 |
| leads | 1.6 | 624 (624-640) | 692 (688-716) | 616.7 (615.2-634) | 371.6 | 0 | 0 | 271.15 | 133 |
| workbooks | 1.7 | 496 (484-500) | 532 (524-556) | 488.6 (479.2-492.6) | 372.7 | 0 | 0 | 233.58 | 75 |
| plugins | 1.6 | 632 (620-632) | 680 (632-688) | 621.6 (611.5-628.5) | 370.7 | 0 | 0 | 275.61 | 96 |

## Cold load, `slow` profile (4x CPU slowdown, 1600 kbit/s, 150 ms RTT)

| route | TTFB ms | FCP ms | LCP ms | heading ready ms | load ms | CLS | TBT ms | JS transferred kB | requests |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| chat | 1.6 | 3344 (3340-3376) | 4776 (4772-4800) | 3344.6 (3339.5-3376.2) | 2160.8 | 0 | 0 | 461.63 | 114 |
| leads | 1.7 | 3216 (3196-3276) | 3488 (3428-3552) | 3211.9 (3191-3273.4) | 2163 | 0.01 | 108 | 271.15 | 133 |
| workbooks | 1.8 | 2624 (2592-2960) | 3144 (3116-3484) | 2623.2 (2590.2-2955.8) | 2167.4 | 0 | 0 | 233.58 | 75 |
| plugins | 2.2 | 3268 (3260-3360) | 3808 (3792-3912) | 3261.6 (3252.3-3353.9) | 2174.5 | 0 | 26 | 275.61 | 96 |

## Interactions and route transitions, `desktop` profile

INP (worst of 11 scripted interactions on the Plugins page): **56 ms** (40-56).

| first visit to | ms until heading painted | JS fetched kB |
|---|---:|---:|
| leads | 86.4 (80.4-89.7) | 43.3 |
| workbooks | 38.9 (38.1-75.2) | 8.88 |
| plugins | 78.5 (75.7-106.5) | 25 |
| analytics | 38 (37.3-51.3) | 10.65 |
| automations | 82.3 (79.3-84.4) | 13.14 |

## Interactions and route transitions, `slow` profile

INP (worst of 10 scripted interactions on the Plugins page): **144 ms** (104-160).

| first visit to | ms until heading painted | JS fetched kB |
|---|---:|---:|
| leads | 682.9 (675.2-692.7) | 191.63 |
| workbooks | 291.1 (284.1-300.9) | 8.88 |
| plugins | 530.6 (506.3-534.3) | 25 |
| analytics | 276.7 (260.1-298.4) | 10.65 |
| automations | 380.4 (362.4-394.6) | 13.14 |

