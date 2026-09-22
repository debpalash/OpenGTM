import { test } from 'node:test';
import assert from 'node:assert/strict';
import { collectAssets, measureAssets } from './route-payload.mjs';

test('counts shared assets once and excludes unopened lazy features', () => {
  const manifest = {
    shell: { file: 'shell.js', imports: ['shared'], dynamicImports: ['other'] },
    route: { file: 'route.js', imports: ['shared'], css: ['route.css', 'shared.css'], dynamicImports: ['csv'] },
    shared: { file: 'shared.js', css: ['shared.css'], imports: ['shell'] },
    other: { file: 'other.js' }, csv: { file: 'csv.js' },
  };
  assert.deepEqual(collectAssets(manifest, ['shell', 'route']),
    ['route.css', 'route.js', 'shared.css', 'shared.js', 'shell.js']);
  assert.ok(collectAssets(manifest, ['shell', 'route', 'csv']).includes('csv.js'));
});

test('missing entries fail loudly instead of undercounting', () => {
  assert.throws(() => collectAssets({}, ['route']), /Missing manifest entry/);
  assert.throws(() => collectAssets({ route: { file: 'route.js', imports: ['missing'] } }, ['route']), /missing/);
});

test('measures bytes rather than character count', () => {
  const result = measureAssets(['unicode.js'], () => Buffer.from('é'));
  assert.equal(result[0].raw, 2);
  assert.ok(result[0].gzip > 0);
});
