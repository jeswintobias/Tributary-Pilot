// The payout worker.
//
// Claims a job, calls the rail, and records what happened — in the same
// transaction that removes the job from the queue, so the two cannot disagree.
//
// WHAT MAKES A SECOND DELIVERY HARMLESS
//
// Leases expire, so a job can be delivered twice: once to a worker that
// stalled and once to its replacement. The protection is not that this never
// happens, it is that paying twice is impossible anyway:
//
//   - the payouts table is keyed by the job's dedupe key, which is derived
//     from the transfer ID, so a second success writes the same row
//   - the rail reference is derived from the transfer, not generated, so a
//     retry presents the same idempotency key the first attempt did
//
// A real rail would be handed that key and would collapse the duplicate on its
// own side. That is the contract this simulation is written to.

import { claim, complete, fail, DEFAULT_LEASE_MS, DEFAULT_MAX_ATTEMPTS } from './queue.js';
import { sendPayout, RailError } from './payout-rail.js';
import { PAYOUT_JOB } from './projector.js';

/**
 * Run one job if one is due. Returns what happened, so a caller can drive the
 * worker one step at a time — which is how the tests run it.
 */
export async function runOnce(db, {
  workerId = `worker-${process.pid}`,
  now = Date.now(),
  leaseMs = DEFAULT_LEASE_MS,
  maxAttempts = DEFAULT_MAX_ATTEMPTS,
  rail = sendPayout,
} = {}) {
  const job = claim(db, workerId, { now, leaseMs });
  if (!job) return { status: 'idle' };

  if (job.kind !== PAYOUT_JOB) {
    // Unknown work. Dead-letter it rather than retrying for ever or dropping
    // it: nobody here knows how to do it, and waiting will not change that.
    fail(db, job, new Error(`no handler for job kind ${job.kind}`), { maxAttempts: 1, now });
    return { status: 'dead-lettered', job, reason: 'unknown kind' };
  }

  const attempt = job.attempts + 1;
  try {
    const { railRef } = await rail(job.payload, attempt);
    complete(db, job, () => {
      db.prepare(
        `INSERT INTO payouts (
           dedupe_key, transfer_id, payee_ref, amount, ccy, state, rail_ref, attempts, completed_at)
         VALUES (?,?,?,?,?,'PAID',?,?,?)
         ON CONFLICT(dedupe_key) DO UPDATE SET
           state = 'PAID', rail_ref = excluded.rail_ref,
           attempts = excluded.attempts, completed_at = excluded.completed_at,
           last_error = NULL`,
      ).run(
        job.dedupe_key, job.payload.transferID, job.payload.payeeRef,
        job.payload.amount, job.payload.ccy, railRef, attempt, Date.now(),
      );
    });
    return { status: 'paid', job, railRef, attempts: attempt };
  } catch (err) {
    // A rail that says "this will never work" is not worth five attempts.
    const attemptsAllowed = (err instanceof RailError && err.retryable === false) ? 1 : maxAttempts;
    const outcome = fail(db, job, err, { maxAttempts: attemptsAllowed, now });
    return {
      status: outcome.deadLettered ? 'dead-lettered' : 'retry',
      job,
      error: String(err.message ?? err),
      attempts: outcome.attempts,
      retryInMs: outcome.retryInMs,
    };
  }
}

/**
 * Keep running until the signal aborts.
 *
 * Polling rather than a notification: SQLite has no push, and at the rate a
 * settlement corridor produces payouts a short poll costs nothing. The polling
 * interval is the latency floor, which is stated rather than hidden.
 */
export async function runWorker(db, { signal, idleMs = 500, onResult, ...options } = {}) {
  while (!signal?.aborted) {
    let result;
    try {
      result = await runOnce(db, options);
    } catch (err) {
      // A failure in the queue machinery itself, not in a job. Pause rather
      // than spin: something is wrong that another immediate attempt will not
      // fix.
      onResult?.({ status: 'error', error: String(err.message ?? err) });
      await sleep(idleMs * 4, signal);
      continue;
    }
    if (result.status !== 'idle') onResult?.(result);
    if (result.status === 'idle') await sleep(idleMs, signal);
  }
}

function sleep(ms, signal) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal?.addEventListener('abort', () => { clearTimeout(t); resolve(); }, { once: true });
  });
}
