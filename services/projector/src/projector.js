// Applying ledger events to the read model.
//
// Everything one event implies happens in a single transaction: the read model
// update, the record that this event has been seen, the checkpoint advance,
// and any work the event creates. See db.js for why that matters.
//
// WHERE THE CHECKPOINT POINTS
//
// It records the block to RESUME FROM, and that is the block of the last
// applied event — not the one after it. A block can hold several events, and
// advancing past the block after applying its first event would lose the rest
// if the process died in between. Resuming from the same block re-delivers
// everything in it, and applied_events makes the second application of each a
// no-op.
//
// So delivery is at-least-once and application is exactly-once, which is the
// only combination available: Fabric can only be asked to resume from a block.

import { enqueue } from './queue.js';
import {
  parsePayload, money, count,
  TRANSFER_EVENTS, EVENT_OBLIGATION, EVENT_WINDOW_SETTLED,
} from './events.js';

/**
 * How far through its life a transfer is.
 *
 * A read model that can go backwards is worse than no read model, because the
 * UI would show a settled transfer reverting to captured and nobody could tell
 * whether the ledger or the projection was wrong. Dedupe should make this
 * unreachable; the rank makes it impossible.
 */
const STATE_RANK = {
  CAPTURED: 1, ACCEPTED: 2, PREPARED: 3, SETTLED: 4, ABORTED: 4,
};

export const PAYOUT_JOB = 'payout';

/** The dedupe key a settled transfer's payout is filed under. */
export function payoutKey(transferID) {
  return `payout:${transferID}`;
}

/**
 * Apply one event. Returns 'applied', or 'skipped' when it has been seen
 * before or is of a kind this projector does not handle.
 *
 * `event` is { name, blockNumber, transactionId, payloadText }.
 */
export function applyEvent(db, event) {
  const { name, blockNumber, transactionId } = event;
  const block = Number(blockNumber);
  const now = Date.now();

  db.exec('BEGIN IMMEDIATE');
  try {
    const seen = db.prepare(
      'SELECT 1 FROM applied_events WHERE block = ? AND tx_id = ? AND event = ?',
    ).get(block, transactionId, name);

    if (seen) {
      // Still move the checkpoint: re-reading this block on every restart
      // would otherwise be permanent.
      bumpCheckpoint(db, block, transactionId, now, { skipped: true });
      db.exec('COMMIT');
      return 'skipped';
    }

    const payload = parsePayload(event.payloadText);
    let handled = true;

    if (TRANSFER_EVENTS.has(name)) {
      applyTransfer(db, name, payload, block, transactionId, now);
    } else if (name === EVENT_OBLIGATION) {
      applyObligation(db, payload, block);
    } else if (name === EVENT_WINDOW_SETTLED) {
      applyWindow(db, payload, block);
    } else {
      // An event this build does not know about. Recording it as seen and
      // moving on is deliberate: a projector that halts on an unfamiliar event
      // stops the read model dead the first time the chaincode grows a feature.
      handled = false;
    }

    db.prepare(
      'INSERT INTO applied_events (block, tx_id, event, applied_at) VALUES (?, ?, ?, ?)',
    ).run(block, transactionId, name, now);
    bumpCheckpoint(db, block, transactionId, now, { skipped: !handled });

    db.exec('COMMIT');
    return handled ? 'applied' : 'skipped';
  } catch (err) {
    try { db.exec('ROLLBACK'); } catch { /* already gone */ }
    throw err;
  }
}

function bumpCheckpoint(db, block, txId, now, { skipped = false } = {}) {
  db.prepare(
    `UPDATE checkpoint
        SET next_block = MAX(next_block, ?),
            last_tx = ?,
            events_applied = events_applied + ?,
            events_skipped = events_skipped + ?,
            updated_at = ?
      WHERE id = 1`,
  ).run(block, txId, skipped ? 0 : 1, skipped ? 1 : 0, now);
}

function applyTransfer(db, name, payload, block, txId, now) {
  const id = payload.transferID;
  if (!id) throw new Error(`${name} carries no transferID`);
  const state = payload.state;
  if (!STATE_RANK[state]) throw new Error(`${name} carries unknown state ${JSON.stringify(state)}`);

  const existing = db.prepare('SELECT state FROM transfers WHERE transfer_id = ?').get(id);

  if (!existing) {
    db.prepare(
      `INSERT INTO transfers (
         transfer_id, state, sender_org, sender_amount, sender_ccy,
         payout_org, payout_amount, payout_ccy, payee_ref, proof_hash,
         first_block, last_block, created_at, updated_at)
       VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
    ).run(
      id, state, payload.senderOrg ?? null, money(payload, 'senderLegAmount'),
      payload.senderCcy ?? null, payload.payoutOrg ?? null,
      money(payload, 'payoutLegAmount'), payload.payoutCcy ?? null,
      payload.payoutPayeeRef ?? null, payload.proofHash || null,
      block, block, count(payload, 'at') || now, now,
    );
  } else if (STATE_RANK[state] >= STATE_RANK[existing.state]) {
    db.prepare(
      `UPDATE transfers
          SET state = ?, proof_hash = COALESCE(NULLIF(?, ''), proof_hash),
              last_block = ?, updated_at = ?
        WHERE transfer_id = ?`,
    ).run(state, payload.proofHash ?? '', block, now, id);
  }
  // A lower-ranked state is dropped rather than written. It can only be a
  // replay, and the newer state is the true one.

  db.prepare(
    `INSERT INTO transfer_history (transfer_id, state, block, tx_id, at)
     VALUES (?,?,?,?,?) ON CONFLICT DO NOTHING`,
  ).run(id, state, block, txId, count(payload, 'at') || now);

  // A settled transfer is the only event that creates work: somebody has to be
  // paid on the far rail. Enqueued here, inside this transaction, so the job
  // exists if and only if the settlement was recorded.
  if (name === 'TransferSettled') {
    enqueue(db, {
      kind: PAYOUT_JOB,
      dedupeKey: payoutKey(id),
      payload: {
        transferID: id,
        payeeRef: payload.payoutPayeeRef ?? null,
        amount: money(payload, 'payoutLegAmount'),
        ccy: payload.payoutCcy ?? null,
        payoutOrg: payload.payoutOrg ?? null,
        block,
      },
    });
  }
}

function applyObligation(db, payload, block) {
  db.prepare(
    `INSERT INTO obligations (obligation_id, ccy, seq, debtor, creditor, amount, block, at)
     VALUES (?,?,?,?,?,?,?,?)
     ON CONFLICT(obligation_id) DO NOTHING`,
  ).run(
    payload.obligationID, payload.ccy, count(payload, 'seq'),
    payload.debtor, payload.creditor, money(payload, 'amount'),
    block, count(payload, 'at'),
  );
}

function applyWindow(db, payload, block) {
  db.prepare(
    `INSERT INTO windows (
       ccy, seq, state, obligations, gross, net, postings,
       compression_bps, closed_by, closed_at, block)
     VALUES (?,?,?,?,?,?,?,?,?,?,?)
     ON CONFLICT(ccy, seq) DO UPDATE SET
       state = excluded.state, obligations = excluded.obligations,
       gross = excluded.gross, net = excluded.net, postings = excluded.postings,
       compression_bps = excluded.compression_bps,
       closed_by = excluded.closed_by, closed_at = excluded.closed_at,
       block = excluded.block`,
  ).run(
    payload.ccy, count(payload, 'seq'), payload.state ?? 'SETTLED',
    count(payload, 'obligations'), money(payload, 'grossValue'),
    money(payload, 'netValue'), count(payload, 'postings'),
    count(payload, 'compressionBps'), payload.closedBy ?? null,
    count(payload, 'closedAt'), block,
  );
}
