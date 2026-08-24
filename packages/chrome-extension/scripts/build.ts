/**
 * Build dist/ for "Load unpacked":
 *   - popup.js / options.js  (ESM, referenced by their HTML pages)
 *   - content.js             (IIFE — content scripts are classic scripts)
 *   - manifest, HTML, and icons copied verbatim from public/
 */
import { cp, mkdir, rm } from "node:fs/promises";
import { join } from "node:path";

const root = join(import.meta.dir, "..");
const dist = join(root, "dist");

await rm(dist, { recursive: true, force: true });
await mkdir(dist, { recursive: true });

async function bundle(entrypoints: string[], format: "esm" | "iife"): Promise<void> {
    const result = await Bun.build({
        entrypoints: entrypoints.map((e) => join(root, "src", e)),
        outdir: dist,
        target: "browser",
        format,
        minify: false,
        sourcemap: "none",
    });
    if (!result.success) {
        for (const log of result.logs) console.error(log);
        process.exit(1);
    }
}

await bundle(["popup.ts", "options.ts"], "esm");
await bundle(["content.ts"], "iife");

await cp(join(root, "public"), dist, { recursive: true });

console.log("Built dist/ — load it via chrome://extensions → Load unpacked.");
