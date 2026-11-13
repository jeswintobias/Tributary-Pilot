// The projector's own store: a checkpoint, a read model, and a work queue, in
// one SQLite file.
//
// WHY ONE FILE AND NOT THREE SERVICES
//
// The three things this holds have to move together. When an event arrives the
// projector must update the read model, advance the checkpoint, and enqueue
// any work the event implies. If those live in different stores there is no
// way to do all three atomically, and every crash lands in one of the bad
// states:
//
//   checkpoint advances, read model does not  -> the event is lost for ever
//   read model updates, checkpoint does not   -> the event is applied twice
//   both update, enqueue does not             -> a settled transfer is never paid
//
// That is the dual-write problem, and it has no solution at the application
// level — retries and compensations only move the window in which it bites.
// Putting all three in one transactional store removes it instead: SQLite
// commits them together or not at all. This is the transactional outbox
// pattern, and the "queue" being a table is the point rather than a shortcut.
//
// At this scale SQLite is also simply the right tool. A broker would add an
// operational dependency and would not make any of the above easier; the
// interface in queue.js is narrow enough that moving to one later is a change
// to that file.
//
// MONEY IS TEXT HERE
//
// Amounts are stored as TEXT, not INTEGER. They are uint64 minor units, which
// exceed what a double holds exactly, and SQLite's INTEGER is signed 64-bit.
// Keeping the decimal string means nothing is rounded on the way through, and
// events.js parses them out of the raw JSON without letting them become
// JavaScript numbers at any point.

import { DatabaseSync } from 'node:sqlite';
import { mkdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const DEFAULT_DB = process.env.PROJECTOR_DB
  ?? path.join(HERE, '..', 'data', 'tributary.db');

const SCHEMA = `
-- Where the projector has read up to. One row, for ever.
CREATE TABLE IF NOT EXISTS checkpoint (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  next_block     INTEGER NOT NULL,
  events_applied INTEGER NOT NULL DEFAULT 0,
  events_skipped INTEGER NOT NULL DEFAULT 0,
  last_tx        TEXT,
  updated_at     INTEGER NOT NULL
);

-- Which events have already been applied. Fabric is resumed from a block, not
-- from an event, so the first block after a restart is re-delivered in full;
-- this is what makes applying it a second time a no-op.
CREATE TABLE IF NOT EXISTS applied_events (
  block      INTEGER NOT NULL,
  tx_id      TEXT NOT NULL,
  event      TEXT NOT NULL,
  applied_at INTEGER NOT NULL,
  PRIMARY KEY (block, tx_id, event)
);

-- Read model: transfers.
CREATE TABLE IF NOT EXISTS transfers (
  transfer_id   TEXT PRIMARY KEY,
  state         TEXT NOT NULL,
  sender_org    TEXT,
  sender_amount TEXT,
  sender_ccy    TEXT,
  payout_org    TEXT,
  payout_amount TEXT,
  payout_ccy    TEXT,
  payee_ref     TEXT,
  proof_hash    TEXT,
  first_block   INTEGER,
  last_block    INTEGER,
  created_at    INTEGER,
  updated_at    INTEGER
);

-- Every state change, kept. The ledger has this too, but reading it back from
-- the ledger costs a round trip per transfer; this is the point of a read
-- model.
CREATE TABLE IF NOT EXISTS transfer_history (
  transfer_id TEXT NOT NULL,
  state       TEXT NOT NULL,
  block       INTEGER NOT NULL,
  tx_id       TEXT NOT NULL,
  at          INTEGER NOT NULL,
  PRIMARY KEY (transfer_id, state, tx_id)
);

-- Read model: netting.
CREATE TABLE IF NOT EXISTS windows (
  ccy             TEXT NOT NULL,
  seq             INTEGER NOT NULL,
  state           TEXT NOT NULL,
  obligations     INTEGER NOT NULL DEFAULT 0,
  gross           TEXT NOT NULL DEFAULT '0',
  net             TEXT NOT NULL DEFAULT '0',
  postings        INTEGER NOT NULL DEFAULT 0,
  compression_bps INTEGER NOT NULL DEFAULT 0,
  closed_by       TEXT,
  closed_at       INTEGER,
  block           INTEGER,
  PRIMARY KEY (ccy, seq)
);

CREATE TABLE IF NOT EXISTS obligations (
  obligation_id TEXT PRIMARY KEY,
  ccy           TEXT NOT NULL,
  seq           INTEGER NOT NULL,
  debtor        TEXT NOT NULL,
  creditor      TEXT NOT NULL,
  amount        TEXT NOT NULL,
  block         INTEGER,
  at            INTEGER
);
CREATE INDEX IF NOT EXISTS obligations_by_window ON obligations (ccy, seq);

-- The queue. A row is a job; claiming one is an UPDATE.
CREATE TABLE IF NOT EXISTS outbox (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  kind         TEXT NOT NULL,
  -- Derived from the event, so the same event enqueued twice is one job. This
  -- is what keeps at-least-once delivery from becoming at-least-once PAYMENT.
  dedupe_key   TEXT NOT NULL UNIQUE,
  payload      TEXT NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  available_at INTEGER NOT NULL,
  leased_until INTEGER,
  leased_by    TEXT,
  last_error   TEXT,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS outbox_ready ON outbox (available_at, leased_until);

-- Jobs that failed too many times. They leave the queue so that one poisonous
-- message cannot block everything behind it, and they are kept rather than
-- dropped because a payment that did not happen is not a thing to discard.
CREATE TABLE IF NOT EXISTS dead_letters (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  payload    TEXT NOT NULL,
  attempts   INTEGER NOT NULL,
  last_error TEXT,
  died_at    INTEGER NOT NULL
);

-- What the worker actually did. Written in the same transaction that removes
-- the job from the queue, so a payout cannot be recorded without the job being
-- finished, nor the job finished without the payout being recorded.
CREATE TABLE IF NOT EXISTS payouts (
  dedupe_key   TEXT PRIMARY KEY,
  transfer_id  TEXT NOT NULL,
  payee_ref    TEXT,
  amount       TEXT,
  ccy          TEXT,
  state        TEXT NOT NULL,
  rail_ref     TEXT,
  attempts     INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT,
  completed_at INTEGER
);
`;

export function openDb(file = DEFAULT_DB) {
  if (file !== ':memory:') mkdirSync(path.dirname(file), { recursive: true });
  const db = new DatabaseSync(file);
  // WAL lets the projector write while the gateway reads, without either
  // blocking the other. The read model exists to be read concurrently.
  db.exec('PRAGMA journal_mode = WAL');
  // Durability over speed: this holds a record of money that moved.
  db.exec('PRAGMA synchronous = FULL');
  db.exec('PRAGMA foreign_keys = ON');
  db.exec('PRAGMA busy_timeout = 5000');
  db.exec(SCHEMA);
  db.prepare(
    `INSERT INTO checkpoint (id, next_block, updated_at)
     VALUES (1, 0, ?) ON CONFLICT(id) DO NOTHING`,
  ).run(Date.now());
  return db;
}

/**
 * Run fn inside one transaction, committing on return and rolling back on
 * throw.
 *
 * Every write path in this service goes through here. The guarantee the whole
 * design rests on is that a read-model update, the checkpoint advance and the
 * enqueue land together.
 */
export function inTransaction(db, fn) {
  db.exec('BEGIN IMMEDIATE');
  try {
    const out = fn();
    db.exec('COMMIT');
    return out;
  } catch (err) {
    try { db.exec('ROLLBACK'); } catch { /* the transaction is already gone */ }
    throw err;
  }
}

export function readCheckpoint(db) {
  return db.prepare('SELECT * FROM checkpoint WHERE id = 1').get();
}
