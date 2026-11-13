// Run the projector against the live ledger until stopped.

import { openDb, readCheckpoint, DEFAULT_DB } from './db.js';
import { applyEvent } from './projector.js';
import { streamEvents, closeLedger, CHANNEL, CHAINCODE, PROJECTOR_MSP } from './ledger.js';
import { queueDepth } from './queue.js';

const db = openDb();
const controller = new AbortController();
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => { controller.abort(); });
}

// PROJECTOR_IDLE_EXIT_MS stops the projector once the stream has been quiet
// for that long. A projector is meant to run for ever, but catching up on a
// backlog and then exiting is what a script wants — and a demo that has to be
// killed with Ctrl-C cannot report its own exit code.
const idleExitMs = Number(process.env.PROJECTOR_IDLE_EXIT_MS ?? 0);
let idleTimer = null;
function resetIdleTimer() {
  if (!idleExitMs) return;
  clearTimeout(idleTimer);
  idleTimer = setTimeout(() => {
    console.log(`\nno events for ${idleExitMs}ms; caught up, stopping`);
    controller.abort();
  }, idleExitMs);
}
resetIdleTimer();

const start = readCheckpoint(db);
console.log(`projector on ${CHANNEL}/${CHAINCODE} as ${PROJECTOR_MSP}`);
console.log(`store ${DEFAULT_DB}`);
console.log(`resuming from block ${start.next_block} `
  + `(${start.events_applied} applied, ${start.events_skipped} skipped so far)`);

let applied = 0;
let skipped = 0;
try {
  for await (const event of streamEvents({ startBlock: start.next_block, signal: controller.signal })) {
    let outcome;
    try {
      outcome = applyEvent(db, event);
    } catch (err) {
      // Stop rather than skip. An event that cannot be applied is a hole in
      // the read model, and carrying on past it would hide the hole behind
      // every event that followed. The checkpoint has not moved, so fixing the
      // cause and restarting replays it.
      console.error(`\nFAILED on ${event.name} in block ${event.blockNumber} `
        + `(tx ${event.transactionId}): ${err.message}`);
      console.error('checkpoint not advanced; fix the cause and restart to replay this block');
      break;
    }
    resetIdleTimer();
    if (outcome === 'applied') applied += 1; else skipped += 1;
    const depth = queueDepth(db);
    console.log(`block ${String(event.blockNumber).padStart(5)}  ${outcome.padEnd(7)}  `
      + `${event.name.padEnd(20)} queue ${depth.ready}/${depth.queued} dead ${depth.dead}`);
  }
} finally {
  clearTimeout(idleTimer);
  const end = readCheckpoint(db);
  console.log(`\nstopped at block ${end.next_block}; `
    + `${applied} applied and ${skipped} skipped this run`);
  closeLedger();
  db.close();
}
