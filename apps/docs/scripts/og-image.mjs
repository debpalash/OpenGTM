// Renders public/og.png (1200×630) from an inline SVG using sharp.
// Run: bun run og   (re-run only when the wordmark or tagline changes)
import sharp from 'sharp';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const lockup = readFileSync(join(here, '../src/assets/opengtm-lockup-dark.svg'), 'utf8');
const lockupB64 = Buffer.from(lockup).toString('base64');

const svg = `
<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#10181c"/>
      <stop offset="1" stop-color="#1b1f4a"/>
    </linearGradient>
  </defs>
  <rect width="1200" height="630" fill="url(#bg)"/>
  <image href="data:image/svg+xml;base64,${lockupB64}" x="90" y="120" width="520" height="140" preserveAspectRatio="xMinYMid meet"/>
  <text x="90" y="340" font-family="Inter, Helvetica, Arial, sans-serif" font-size="44" font-weight="700" fill="#ffffff">GTM agents for the world.</text>
  <text x="90" y="400" font-family="Inter, Helvetica, Arial, sans-serif" font-size="26" fill="#b9c6cb">Open-source, self-hostable alternative to Clay.</text>
  <text x="90" y="440" font-family="Inter, Helvetica, Arial, sans-serif" font-size="26" fill="#b9c6cb">Source · enrich · research · push to CRM — with your own keys.</text>
  <text x="90" y="550" font-family="Inter, Helvetica, Arial, sans-serif" font-size="24" fill="#20CFAF">opengtm.palash.dev</text>
</svg>`;

const png = await sharp(Buffer.from(svg)).png({ compressionLevel: 9 }).toBuffer();
writeFileSync(join(here, '../public/og.png'), png);
console.log(`wrote public/og.png (${png.length} bytes)`);
