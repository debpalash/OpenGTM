// Rasterises the OpenGTM mark into PNG favicons. Run: bun run favicons
import sharp from 'sharp';
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const mark = readFileSync(join(here, '../src/assets/opengtm-mark.svg'));
const out = (name, buf) => writeFileSync(join(here, '../public', name), buf);

// Transparent, padded a little so the strokes never touch the tile edge.
for (const [name, size] of [['icon-192.png', 192], ['icon-512.png', 512], ['favicon-32.png', 32]]) {
  out(name, await sharp(mark, { density: 384 }).resize(size, size, { fit: 'contain', background: { r: 0, g: 0, b: 0, alpha: 0 } }).png().toBuffer());
}
// iOS ignores transparency: give the touch icon the site's dark ground.
const touch = await sharp(mark, { density: 384 }).resize(140, 140).png().toBuffer();
out('apple-touch-icon.png', await sharp({ create: { width: 180, height: 180, channels: 4, background: '#12141d' } })
  .composite([{ input: touch, gravity: 'centre' }]).png().toBuffer());
console.log('favicons written');
