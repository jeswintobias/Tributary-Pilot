// Client for the FX oracle service.
//
// The gateway does not hold the oracle's signing key and must not. Anyone with
// it can sign any rate and the chaincode will believe it, so the key lives in
// one process and everything else asks that process for a quote.
//
// A transfer cannot be created without one, so an oracle that is down stops
// new transfers. That is the correct failure: settling at an unverified rate
// is worse than not settling.

const ORACLE_URL = process.env.ORACLE_URL ?? 'http://127.0.0.1:7710';
const TIMEOUT_MS = Number(process.env.ORACLE_TIMEOUT_MS ?? 5000);

export class OracleUnavailable extends Error {
  constructor(message) {
    super(message);
    this.name = 'OracleUnavailable';
  }
}

async function call(path, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
  try {
    const res = await fetch(`${ORACLE_URL}${path}`, { ...options, signal: controller.signal });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new OracleUnavailable(body.error ?? `oracle returned ${res.status}`);
    return body;
  } catch (err) {
    if (err instanceof OracleUnavailable) throw err;
    throw new OracleUnavailable(
      `the FX oracle at ${ORACLE_URL} is unreachable (${err.message}). `
      + 'Start it with: cd services/oracle && npm start');
  } finally {
    clearTimeout(timer);
  }
}

/** The oracle's identity and key, for RegisterOracle. */
export function oracleIdentity() {
  return call('/pubkey');
}

/**
 * Ask for a signed quote.
 *
 * `maxSlippageBps` is how far the transfer's actual legs may sit from the
 * quoted rate. It is part of the signed quote, so the tolerance is the
 * oracle's statement rather than something a client can widen afterwards.
 */
export function requestQuote({ baseCcy, quoteCcy, amount, maxSlippageBps = 0, ttlSeconds = 300 }) {
  return call('/quote', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ baseCcy, quoteCcy, amount, maxSlippageBps, ttlSeconds }),
  });
}

export const ORACLE_ENDPOINT = ORACLE_URL;
