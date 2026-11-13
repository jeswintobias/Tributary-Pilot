// A durable work queue in the same database as the read model.
//
// The operations are deliberately few, because every one of them is a place
// where a job could be lost or run twice:
//
//   enqueue  - insert, ignoring a duplicate dedupe key
//   claim    - take the oldest due job and lease it
//   complete - delete it, together with whatever the handler wrote
//   fail     - put it back with a longer delay, or dead-letter it
//
// LEASES, NOT LOCKS
//
// A claimed job is leased until a deadline rather than locked. A worker that
// crashes holds nothing: its lease simply expires and another worker picks the
// job up. A lock would need the crashed process to come back and release it,
// which is the one thing a crashed process cannot be relied on to do.
//
// The cost of a lease is that a job can run twice — once by a worker that
// stalled past its deadline and once by its replacement. That is why handlers
// must be idempotent and why the payout record is keyed by the dedupe key.
// There is no setting that removes this; at-least-once with idempotent
// handlers is the honest shape of the problem.

export const DEFAULT_MAX_ATTEMPTS = 5;
export const DEFAULT_LEASE_MS = 30_000;

/**
 * Backoff before a failed job is retried: 1s, 2s, 4s, 8s, 16s, capped.
 *
 * Deterministic rather than jittered because there is one worker here and a
 * reproducible test is worth more than spreading load that does not exist.
 * With several workers this wants jitter, or they retry in lockstep.
 */
export function backoffMs(attempts) {
  return Math.min(1000 * 2 ** Math.max(0, attempts - 1), 60_000);
}

/**
 * Add a job unless one with the same dedupe key is already queued.
 *
 * Called inside the projector's transaction, so a job appears only if the
 * event that caused it was also applied.
 */
export function enqueue(db, { kind, dedupeKey, payload, availableAt = Date.now() }) {
  const result = db.prepare(
    `INSERT INTO outbox (kind, dedupe_key, payload, available_at, created_at)
     VALUES (?, ?, ?, ?, ?)
     ON CONFLICT(dedupe_key) DO NOTHING`,
  ).run(kind, dedupeKey, JSON.stringify(payload), availableAt, Date.now());
  return result.changes > 0;
}

/**
 * Take the oldest job that is due and not currently leased.
 *
 * The SELECT and the UPDATE run in one transaction, so two workers cannot
 * claim the same row: the second finds it already leased.
 */
export function claim(db, workerId, { now = Date.now(), leaseMs = DEFAULT_LEASE_MS } = {}) {
  db.exec('BEGIN IMMEDIATE');
  try {
    const job = db.prepare(
      `SELECT * FROM outbox
        WHERE available_at <= ?
          AND (leased_until IS NULL OR leased_until <= ?)
        ORDER BY available_at, id
        LIMIT 1`,
    ).get(now, now);
    if (!job) {
      db.exec('COMMIT');
      return null;
    }
    db.prepare('UPDATE outbox SET leased_until = ?, leased_by = ? WHERE id = ?')
      .run(now + leaseMs, workerId, job.id);
    db.exec('COMMIT');
    return { ...job, payload: JSON.parse(job.payload) };
  } catch (err) {
    try { db.exec('ROLLBACK'); } catch { /* already gone */ }
    throw err;
  }
}

/**
 * Finish a job and record its result together.
 *
 * `write` runs in the same transaction that removes the row, so there is no
 * window in which the payout is recorded but the job is still queued, or the
 * job is gone but nothing records why.
 */
export function complete(db, job, write) {
  db.exec('BEGIN IMMEDIATE');
  try {
    write?.();
    db.prepare('DELETE FROM outbox WHERE id = ?').run(job.id);
    db.exec('COMMIT');
  } catch (err) {
    try { db.exec('ROLLBACK'); } catch { /* already gone */ }
    throw err;
  }
}

/**
 * Record a failure: retry later, or dead-letter once the attempts run out.
 *
 * Dead-lettering is what stops one unprocessable job from holding up every
 * job behind it. The row is moved, not deleted — a payout that did not happen
 * is something somebody has to look at.
 */
export function fail(db, job, error, { maxAttempts = DEFAULT_MAX_ATTEMPTS, now = Date.now() } = {}) {
  const attempts = job.attempts + 1;
  const message = String(error?.message ?? error).slice(0, 500);
  db.exec('BEGIN IMMEDIATE');
  try {
    if (attempts >= maxAttempts) {
      db.prepare(
        `INSERT INTO dead_letters (kind, dedupe_key, payload, attempts, last_error, died_at)
         VALUES (?, ?, ?, ?, ?, ?)`,
      ).run(job.kind, job.dedupe_key, JSON.stringify(job.payload), attempts, message, now);
      db.prepare(
        `INSERT INTO payouts (dedupe_key, transfer_id, payee_ref, amount, ccy, state, attempts, last_error)
         VALUES (?, ?, ?, ?, ?, 'DEAD', ?, ?)
         ON CONFLICT(dedupe_key) DO UPDATE SET
           state = 'DEAD', attempts = excluded.attempts, last_error = excluded.last_error`,
      ).run(
        job.dedupe_key, job.payload.transferID ?? '', job.payload.payeeRef ?? null,
        job.payload.amount ?? null, job.payload.ccy ?? null, attempts, message,
      );
      db.prepare('DELETE FROM outbox WHERE id = ?').run(job.id);
      db.exec('COMMIT');
      return { deadLettered: true, attempts };
    }
    db.prepare(
      `UPDATE outbox
          SET attempts = ?, available_at = ?, leased_until = NULL, leased_by = NULL, last_error = ?
        WHERE id = ?`,
    ).run(attempts, now + backoffMs(attempts), message, job.id);
    db.exec('COMMIT');
    return { deadLettered: false, attempts, retryInMs: backoffMs(attempts) };
  } catch (err) {
    try { db.exec('ROLLBACK'); } catch { /* already gone */ }
    throw err;
  }
}

export function queueDepth(db, now = Date.now()) {
  return {
    queued: db.prepare('SELECT COUNT(*) AS n FROM outbox').get().n,
    ready: db.prepare(
      `SELECT COUNT(*) AS n FROM outbox
        WHERE available_at <= ? AND (leased_until IS NULL OR leased_until <= ?)`,
    ).get(now, now).n,
    leased: db.prepare('SELECT COUNT(*) AS n FROM outbox WHERE leased_until > ?').get(now).n,
    dead: db.prepare('SELECT COUNT(*) AS n FROM dead_letters').get().n,
  };
}
