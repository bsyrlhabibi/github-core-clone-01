// Entry point of the forge, written for the make.fun
// github-core-clone-01 entry.
//
// The graded server is a native Go program: its source lives in
// this repository and it is compiled ahead of time to the
// statically linked `forge-core` binary committed beside this
// file. Node's only job is to be the launcher the harness
// starts — it stages the binary somewhere it can be executed,
// spawns it, and then stays out of the way.
//
// Why the staging copy: the binary travels through git, and not
// every path it can arrive by preserves its executable bit. A
// file that comes back as mode 644 cannot be spawned, so the
// launcher copies it into the temp directory under a
// per-process name and sets the bit there. That makes the
// launch work no matter how the file arrived.
//
// Everything else is inherited. The child receives this
// process's environment (MAKEFUN_ENV_URL, MAKEFUN_SHOP_TOKEN
// and friends) and its stdio; SIGINT and SIGTERM sent to the
// launcher are forwarded to the child; and the launcher's exit
// code is the child's exit code, so the harness sees exactly
// the verdict the Go program reached.

import { spawn } from 'node:child_process';
import { chmodSync, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const committed = join(here, 'forge-core');
const staged = join(tmpdir(), 'forge-core-' + String(process.pid));

try {
  copyFileSync(committed, staged);
  chmodSync(staged, 0o755);
} catch (err) {
  const detail = err instanceof Error ? err.message : String(err);
  process.stderr.write('forge entry: cannot stage the forge-core binary: ' + detail + '\n');
  process.exit(1);
}

const child = spawn(staged, [], { stdio: 'inherit' });

process.on('SIGINT', () => {
  child.kill('SIGINT');
});
process.on('SIGTERM', () => {
  child.kill('SIGTERM');
});

child.on('error', (err) => {
  process.stderr.write('forge entry: forge-core failed to start: ' + err.message + '\n');
  process.exit(1);
});

child.on('exit', (code) => {
  process.exit(code === null ? 1 : code);
});
