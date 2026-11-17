// Where the rates come from.
//
// SIMULATED. This reads no market feed. A real oracle would take a rate from a
// licensed source and would be answerable for it; this one quotes from a fixed
// table so that a demo produces sensible numbers, and says so everywhere it
// appears.
//
// Rates are integer numerator over integer denominator, never decimals. The
// chaincode requires it and the reason is determinism: two endorsing peers
// must compute the same expected payout, and 22.7431 is not a number a float
// represents exactly.

/** AED->INR and a few others, as exact rationals. */
const TABLE = {
  'AED/INR': { rateNum: 227431n, rateDen: 10000n },
  'USD/INR': { rateNum: 835120n, rateDen: 10000n },
  'AED/USD': { rateNum: 2723n, rateDen: 10000n },
  'INR/AED': { rateNum: 10000n, rateDen: 227431n },
};

export function pairKey(baseCcy, quoteCcy) {
  return `${baseCcy}/${quoteCcy}`;
}

export function knownPairs() {
  return Object.keys(TABLE);
}

/**
 * The rate for a pair.
 *
 * `driftBps` moves the quote deterministically from the table value, so a demo
 * can show slippage and stale-quote handling without pretending the rate moved
 * on its own. Deterministic, because a random rate would make a test that
 * checks the arithmetic impossible to write.
 */
export function rateFor(baseCcy, quoteCcy, { driftBps = 0 } = {}) {
  const entry = TABLE[pairKey(baseCcy, quoteCcy)];
  if (!entry) {
    throw new Error(
      `no rate for ${baseCcy}/${quoteCcy}; this oracle quotes ${knownPairs().join(', ')}`);
  }
  if (!driftBps) return { rateNum: entry.rateNum.toString(), rateDen: entry.rateDen.toString() };

  // Apply the drift to the numerator, keeping everything integral.
  const drift = BigInt(driftBps);
  const num = (entry.rateNum * (10000n + drift)) / 10000n;
  return { rateNum: num.toString(), rateDen: entry.rateDen.toString() };
}
