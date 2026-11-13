// A simulated payout rail.
//
// This stands in for UPI or e-rupee. It is simulated and labelled as such
// everywhere it appears, because the real thing needs a licensed counterparty
// and nothing here should suggest otherwise.
//
// What it is actually for is making the failure paths real. A rail that always
// succeeds proves nothing about retries, backoff or dead-lettering, so this one
// fails on demand and deterministically:
//
//   PAYOUT_FAIL_REFS   payee references that fail every time (poison)
//   PAYOUT_FLAKY_REFS  payee references that fail until their final attempt
//   PAYOUT_LATENCY_MS  artificial delay per call
//
// Deterministic rather than random, so a test asserting "this one reaches the
// dead-letter table and that one recovers on retry" is a test and not a
// coin toss.

const list = (name) => new Set(
  (process.env[name] ?? '').split(',').map((s) => s.trim()).filter(Boolean),
);

export class RailError extends Error {
  constructor(message, { retryable = true } = {}) {
    super(message);
    this.name = 'RailError';
    this.retryable = retryable;
  }
}

/**
 * Send a payout. Resolves with a rail reference, or throws RailError.
 *
 * `attempt` is 1-based and lets the flaky set model a transient outage that
 * clears, which is the case retries exist for.
 */
export async function sendPayout({ transferID, payeeRef, amount, ccy }, attempt = 1, env = process.env) {
  const latency = Number(env.PAYOUT_LATENCY_MS ?? 0);
  if (latency > 0) await new Promise((r) => setTimeout(r, latency));

  const alwaysFail = list('PAYOUT_FAIL_REFS');
  const flaky = list('PAYOUT_FLAKY_REFS');
  const flakyUntil = Number(env.PAYOUT_FLAKY_ATTEMPTS ?? 3);

  if (!payeeRef) {
    // Nothing to retry: no amount of waiting produces a beneficiary.
    throw new RailError('no payee reference on the transfer', { retryable: false });
  }
  if (alwaysFail.has(payeeRef)) {
    throw new RailError(`rail rejected ${payeeRef}: account closed`);
  }
  if (flaky.has(payeeRef) && attempt < flakyUntil) {
    throw new RailError(`rail timed out for ${payeeRef} on attempt ${attempt}`);
  }
  if (!/^\d+$/.test(String(amount)) || BigInt(amount) <= 0n) {
    throw new RailError(`refusing to pay ${amount} ${ccy}`, { retryable: false });
  }

  // A reference the rail would have given back. Derived from the transfer so
  // it is stable across retries — a real rail's idempotency key looks like
  // this, and a random one would make a retry indistinguishable from a second
  // payment.
  return { railRef: `SIM-${ccy}-${transferID}` };
}
