// The query side.
//
// Every other endpoint in this service reads the ledger directly: a query per
// request, answered by a peer. That is correct and it does not scale — a
// dashboard refreshing every few seconds turns into sustained load on the
// peers whose actual job is endorsing transactions, and a query can only ever
// return what a peer will compute on demand, never "how many transfers settled
// last week".
//
// So reads that do not need to be authoritative come from the projector's
// store instead. That store is built by replaying the chain, so it holds the
// same facts; what it does not hold is anything newer than the last event the
// projector applied. The gap is real and the API reports it rather than hiding
// it, because a dashboard showing stale numbers as if they were live is worse
// than one that says how stale they are.
//
// Opened READ-ONLY. This process must not be able to corrupt the projector's
// store, and a bug here that wrote to it would be very hard to notice.

import { DatabaseSync } from 'node:sqlite';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const PROJECTION_DB = process.env.PROJECTOR_DB
  ?? path.join(HERE, '..', '..', 'projector', 'data', 'tributary.db');

let handle = null;

/**
 * Open the projection if it exists.
 *
 * A missing file is an ordinary state, not an error: the projector is a
 * separate process and may simply not have been started. Callers get null and
 * say so.
 */
function db() {
  if (handle) return handle;
  if (!existsSync(PROJECTION_DB)) return null;
  try {
    handle = new DatabaseSync(PROJECTION_DB, { readOnly: true });
    return handle;
  } catch {
    return null;
  }
}

export function projectionAvailable() {
  return db() !== null;
}

/**
 * Everything the pipeline view needs, in one read.
 *
 * `lagBlocks` is the honest part: how far the projection is behind the ledger,
 * given the ledger's current height. The caller supplies that height because
 * only it can ask a peer.
 */
export function pipelineState({ ledgerBlock = null } = {}) {
  const d = db();
  if (!d) {
    return {
      available: false,
      reason: 'the projector has not run yet; start it with "npm start" in services/projector',
      path: PROJECTION_DB,
    };
  }

  const checkpoint = d.prepare('SELECT * FROM checkpoint WHERE id = 1').get();
  const now = Date.now();
  const depth = {
    queued: d.prepare('SELECT COUNT(*) AS n FROM outbox').get().n,
    ready: d.prepare(
      `SELECT COUNT(*) AS n FROM outbox
        WHERE available_at <= ? AND (leased_until IS NULL OR leased_until <= ?)`,
    ).get(now, now).n,
    leased: d.prepare('SELECT COUNT(*) AS n FROM outbox WHERE leased_until > ?').get(now).n,
    dead: d.prepare('SELECT COUNT(*) AS n FROM dead_letters').get().n,
  };

  return {
    available: true,
    path: PROJECTION_DB,
    checkpoint: {
      nextBlock: checkpoint.next_block,
      applied: checkpoint.events_applied,
      skipped: checkpoint.events_skipped,
      updatedAt: checkpoint.updated_at,
      // null when the ledger height is unknown, never a guess.
      lagBlocks: ledgerBlock === null ? null : Math.max(0, ledgerBlock - checkpoint.next_block),
      staleMs: checkpoint.updated_at ? now - checkpoint.updated_at : null,
    },
    queue: depth,
    payouts: d.prepare(
      `SELECT dedupe_key, transfer_id, payee_ref, amount, ccy, state, rail_ref,
              attempts, last_error, completed_at
         FROM payouts ORDER BY completed_at DESC NULLS LAST LIMIT 25`,
    ).all(),
    deadLetters: d.prepare(
      'SELECT id, kind, dedupe_key, attempts, last_error, died_at FROM dead_letters ORDER BY died_at DESC LIMIT 25',
    ).all(),
    pending: d.prepare(
      `SELECT id, kind, dedupe_key, attempts, available_at, leased_by, last_error
         FROM outbox ORDER BY available_at LIMIT 25`,
    ).all(),
    transfers: d.prepare(
      `SELECT transfer_id, state, sender_amount, sender_ccy, payout_amount, payout_ccy,
              payee_ref, first_block, last_block
         FROM transfers ORDER BY last_block DESC LIMIT 50`,
    ).all(),
    windowTotals: d.prepare(
      `SELECT COUNT(*) AS windows, COALESCE(SUM(obligations), 0) AS obligations,
              COALESCE(SUM(postings), 0) AS postings
         FROM windows`,
    ).get(),
  };
}

export function closeProjection() {
  try { handle?.close(); } catch { /* already closed */ }
  handle = null;
}
