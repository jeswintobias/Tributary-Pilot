// Run the payout worker until stopped.

import { openDb, DEFAULT_DB } from './db.js';
import { runWorker } from './worker.js';
import { queueDepth } from './queue.js';

const db = openDb();
const controller = new AbortController();
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => { controller.abort(); });
}

const workerId = `worker-${process.pid}`;
console.log(`payout worker ${workerId}`);
console.log(`store ${DEFAULT_DB}`);
console.log('rail: SIMULATED. Nothing here touches a real payment network.');
const failing = process.env.PAYOUT_FAIL_REFS;
const flaky = process.env.PAYOUT_FLAKY_REFS;
if (failing) console.log(`PAYOUT_FAIL_REFS=${failing} — these will dead-letter`);
if (flaky) console.log(`PAYOUT_FLAKY_REFS=${flaky} — these fail then recover`);

// WORKER_IDLE_EXIT_MS stops the worker once there has been nothing to do for
// that long, so a script can drain the queue and then check an exit code.
// Jobs waiting on a backoff still count as work, or the worker would walk out
// in the gap between a failure and its retry and leave the job sitting there.
const idleExitMs = Number(process.env.WORKER_IDLE_EXIT_MS ?? 0);
let idleSince = Date.now();
if (idleExitMs) {
  const tick = setInterval(() => {
    if (queueDepth(db).queued > 0) { idleSince = Date.now(); return; }
    if (Date.now() - idleSince >= idleExitMs) {
      console.log(`\nqueue empty for ${idleExitMs}ms; stopping`);
      clearInterval(tick);
      controller.abort();
    }
  }, 250);
  tick.unref?.();
}

await runWorker(db, {
  signal: controller.signal,
  workerId,
  onResult: (r) => {
    idleSince = Date.now();
    const depth = queueDepth(db);
    const id = r.job?.payload?.transferID ?? '-';
    const detail = r.status === 'paid' ? `ref ${r.railRef}`
      : r.status === 'retry' ? `attempt ${r.attempts}, retry in ${r.retryInMs}ms: ${r.error}`
        : r.error ?? '';
    console.log(`${r.status.padEnd(14)} ${String(id).padEnd(24)} ${detail}  `
      + `[queue ${depth.ready}/${depth.queued} dead ${depth.dead}]`);
  },
});

console.log('\nworker stopped');
db.close();
