import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const recipe = dirname(fileURLToPath(import.meta.url));
const repository = resolve(recipe, '../../..');
const revision = 'e99fd1a7683f37a957852c3f7ef8f6a29732bb38';
const expectedHash = '5f21ab5f3122251b9a2334df86371d1507888db682e7a7411b2b895e23b19875';
const workspace = mkdtempSync(join(tmpdir(), 'opengtm-twenty-build-'));

function run(command, args, cwd = workspace) {
  execFileSync(command, args, { cwd, stdio: 'inherit' });
}

console.log(`Building MIT Twenty UI ${revision} in ${workspace}`);
run('git', ['init', '--quiet']);
run('git', ['remote', 'add', 'origin', 'https://github.com/twentyhq/twenty.git']);
run('git', ['sparse-checkout', 'set', 'packages/twenty-ui']);
run('git', ['fetch', '--filter=blob:none', '--depth', '1', 'origin', revision]);
run('git', ['checkout', '--detach', revision]);

const source = join(workspace, 'packages/twenty-ui');
copyFileSync(join(recipe, 'build-package.json'), join(source, 'package.json'));
copyFileSync(join(recipe, 'build-lock.json'), join(source, 'package-lock.json'));
run('npm', ['ci', '--workspaces=false', '--ignore-scripts', '--no-audit', '--no-fund'], source);
run('npm', ['run', 'build'], source);
run(join(source, 'node_modules/.bin/tsx'), ['scripts/checkOptionalDependencies.ts'], source);
const packed = JSON.parse(execFileSync('npm', ['pack', '--workspaces=false', '--json'], {
  cwd: source,
  encoding: 'utf8',
}))[0];
const artifact = join(source, packed.filename);
const actualHash = createHash('sha256').update(readFileSync(artifact)).digest('hex');
if (actualHash !== expectedHash) {
  throw new Error(`Artifact differs: expected ${expectedHash}, received ${actualHash}. Inspect ${workspace}; do not silently replace the pin.`);
}
const destination = join(repository, 'apps/web/vendor/twenty-ui-2.42.0-opengtm.tgz');
mkdirSync(dirname(destination), { recursive: true });
copyFileSync(artifact, destination);
console.log(`Verified ${actualHash}\nWrote ${destination}\nBuild workspace retained: ${workspace}`);
