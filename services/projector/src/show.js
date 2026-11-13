// Print the read model. This is what a dashboard reads instead of the ledger.

import { openDb, readCheckpoint, DEFAULT_DB } from './db.js';
import { queueDepth } from './queue.js';

const db = openDb();
const cp = readCheckpoint(db);
const depth = queueDepth(db);

console.log(`store ${DEFAULT_DB}`);
console.log(`checkpoint: resume from block ${cp.next_block}, `
  + `${cp.events_applied} applied, ${cp.events_skipped} skipped`);
console.log(`queue: ${depth.ready} ready of ${depth.queued}, ${depth.leased} leased, ${depth.dead} dead\n`);

const transfers = db.prepare('SELECT * FROM transfers ORDER BY first_block').all();
console.log(`TRANSFERS (${transfers.length})`);
for (const t of transfers) {
  console.log(`  ${t.transfer_id.padEnd(26)} ${t.state.padEnd(9)} `
    + `${t.sender_amount} ${t.sender_ccy} -> ${t.payout_amount} ${t.payout_ccy}  blocks ${t.first_block}-${t.last_block}`);
}

const windows = db.prepare('SELECT * FROM windows ORDER BY ccy, seq').all();
console.log(`\nNETTING WINDOWS (${windows.length})`);
for (const w of windows) {
  console.log(`  ${w.ccy} ${w.seq}  ${String(w.obligations).padStart(2)} obligations  `
    + `gross ${String(w.gross).padStart(9)}  net ${String(w.net).padStart(9)}  `
    + `${w.postings} posting(s)  ${(w.compression_bps / 100).toFixed(2)}% saved  by ${w.closed_by}`);
}

const obligations = db.prepare('SELECT COUNT(*) AS n FROM obligations').get().n;
console.log(`\nOBLIGATIONS: ${obligations}`);

const payouts = db.prepare('SELECT * FROM payouts ORDER BY completed_at').all();
console.log(`\nPAYOUTS (${payouts.length})  — SIMULATED rail`);
for (const p of payouts) {
  console.log(`  ${p.transfer_id.padEnd(26)} ${p.state.padEnd(5)} `
    + `${String(p.amount).padStart(10)} ${p.ccy}  ${p.rail_ref ?? p.last_error ?? ''} (attempt ${p.attempts})`);
}

const dead = db.prepare('SELECT * FROM dead_letters').all();
if (dead.length) {
  console.log(`\nDEAD LETTERS (${dead.length})`);
  for (const d of dead) console.log(`  ${d.dedupe_key}  after ${d.attempts} attempts: ${d.last_error}`);
}
db.close();
