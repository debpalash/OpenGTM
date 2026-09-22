import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

// Follow static dependencies only. Dynamic imports belong to a later interaction
// unless explicitly supplied as an entry (for example, the active route).
export function collectAssets(manifest, entries) {
  const visited = new Set();
  const assets = new Set();
  function visit(key) {
    if (visited.has(key)) return;
    const chunk = manifest[key];
    if (!chunk) throw new Error(`Missing manifest entry: ${key}`);
    visited.add(key);
    assets.add(chunk.file);
    for (const css of chunk.css ?? []) assets.add(css);
    for (const dependency of chunk.imports ?? []) visit(dependency);
  }
  entries.forEach(visit);
  return [...assets].sort();
}

export function measureAssets(files, read) {
  return files.map(file => {
    const bytes = read(file);
    return { file, raw: bytes.length, gzip: gzipSync(bytes).length };
  }).sort((a, b) => b.gzip - a.gzip);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const dist = resolve(process.argv[2] ?? 'apps/web/dist');
  const route = process.argv[3] ?? 'src/pages/workbook-editor.tsx';
  const manifest = JSON.parse(readFileSync(resolve(dist, '.vite/manifest.json'), 'utf8'));
  const files = collectAssets(manifest, ['index.html', route]);
  const measured = measureAssets(files, file => readFileSync(resolve(dist, file)));
  const totals = {};
  for (const kind of ['js', 'css']) {
    const matching = measured.filter(asset => asset.file.endsWith(`.${kind}`));
    totals[kind] = {
      files: matching.length,
      raw: matching.reduce((sum, asset) => sum + asset.raw, 0),
      gzip: matching.reduce((sum, asset) => sum + asset.gzip, 0),
    };
  }
  console.log(JSON.stringify({ route, totals, assets: measured,
    scope: 'Static shell + route JS/CSS; excludes fonts, images, API data and unopened dynamic imports. Gzip is an estimate, not observed network transfer.'
  }, null, 2));
}
