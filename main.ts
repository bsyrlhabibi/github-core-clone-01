// Entry point of the forge, written for the make.fun github-core-clone-01
// entry: the graded surface lives in app.ts and its modules, while this
// file only owns the process lifecycle around it.
//
// The harness that grades this program talks to it through one env var,
// MAKEFUN_ENV_URL. The whole job of this file is the conversation with
// that harness, in four moves:
//
//   1. Build the HTTP app (app.ts) and bind port 0, so the kernel picks
//      whichever port is free. Port 8080 belongs to the harness itself
//      and is never ours to take.
//   2. Announce the chosen port with POST /register and a {"port": N}
//      body. From then on the harness reaches the forge at that port.
//   3. Ask GET /done every so often. While the grading suite runs, the
//      answer is {"done": false}; the first {"done": true} ends us.
//   4. Close the server and exit 0.
//
// A missing MAKEFUN_ENV_URL means there is no harness to talk to, which
// is a contract error: report it on stderr and exit 1 immediately.
// Registering is retried with a growing delay because the harness may
// still be booting; a 409 from /register only means an earlier register
// already won, and a 400 or 405 means the request itself is wrong, so
// those two are fatal instead of retried.

import { createApp } from './app.ts';

function harnessBase(): string {
  const raw = process.env.MAKEFUN_ENV_URL;
  if (raw === undefined) return '';
  return raw.replace(/\/+$/, '');
}

const base = harnessBase();

if (base === '') {
  process.stderr.write('forge entry: MAKEFUN_ENV_URL is missing, so there is no harness to register with\n');
  process.exit(1);
}

// One harness call may hang for at most this long before we give up on
// that attempt (never on the conversation as a whole).
const CALL_TIMEOUT_MS = 30_000;

// The pause between two attempts starts here and doubles each round,
// but never grows past the ceiling.
const FIRST_PAUSE_MS = 250;
const LONGEST_PAUSE_MS = 2_000;

function pauseAfter(failures: number): number {
  const grown = FIRST_PAUSE_MS * 2 ** Math.min(failures, 8);
  return Math.min(LONGEST_PAUSE_MS, grown);
}

function wait(ms: number): Promise<void> {
  return new Promise((done) => {
    setTimeout(done, ms);
  });
}

async function announcePort(port: number): Promise<void> {
  let failures = 0;
  for (;;) {
    let verdict: 'accepted' | 'fatal' | 'again' = 'again';
    try {
      const reply = await fetch(base + '/register', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port: port }),
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      });
      if (reply.status === 200 || reply.status === 409) {
        verdict = 'accepted';
      } else if (reply.status === 400 || reply.status === 405) {
        const detail = await reply.text();
        throw new Error('register refused the entry with HTTP ' + reply.status + ': ' + detail);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register refused')) throw err;
      verdict = 'again';
    }
    if (verdict === 'accepted') return;
    await wait(pauseAfter(failures));
    failures += 1;
  }
}

async function harnessFinished(): Promise<boolean> {
  const reply = await fetch(base + '/done', {
    signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
  });
  if (!reply.ok) return false;
  const body: unknown = await reply.json();
  if (typeof body !== 'object' || body === null) return false;
  return (body as { done?: unknown }).done === true;
}

async function waitUntilFinished(): Promise<void> {
  let failures = 0;
  for (;;) {
    let finished = false;
    try {
      finished = await harnessFinished();
    } catch {
      finished = false;
    }
    if (finished) return;
    await wait(pauseAfter(failures));
    failures += 1;
  }
}

async function run(): Promise<void> {
  const server = createApp();

  const bound = new Promise<number>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        reject(new Error('the forge did not land on a TCP port'));
        return;
      }
      resolve(address.port);
    });
  });

  const port = await bound;
  process.stdout.write('forge entry: serving on port ' + String(port) + '\n');

  await announcePort(port);
  process.stdout.write('forge entry: registered with the harness; idling until it is done\n');

  await waitUntilFinished();

  server.close();
  process.stdout.write('forge entry: harness is done; shutting down\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  const message = err instanceof Error ? err.message : String(err);
  process.stderr.write('forge entry: ' + message + '\n');
  process.exit(1);
}
