// Who is allowed to call, and as whom.
//
// The rule that matters: a token maps to EXACTLY ONE organisation. The caller
// does not get to say which identity to act as — before this, the request body
// carried `as: "Org2MSP"` and the service believed it, so anyone who could
// reach the port could move the payout bank's money.
//
// In production each organisation runs its own gateway holding only its own
// key, and there is one token. Three tokens here is a single-operator demo
// convenience, and `/api/dev/tokens` exposes them only under the conditions in
// devTokensAllowed() below.

import { promises as fs } from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const TOKEN_FILE = process.env.GATEWAY_TOKEN_FILE ?? path.join(HERE, '..', '.tokens.json');

export const DEV_MODE = process.env.GATEWAY_DEV === '1';

/** token -> mspId */
let tokens = new Map();

/**
 * Load tokens from GATEWAY_TOKENS, else the token file, else generate a set
 * and write it out.
 *
 * Generating beats defaulting to something guessable: a service that invents a
 * strong secret on first boot is safe even when nobody reads the setup notes.
 */
export async function loadTokens(mspIds) {
  const fromEnv = process.env.GATEWAY_TOKENS;
  if (fromEnv) {
    tokens = new Map(Object.entries(JSON.parse(fromEnv)));
    return { source: 'GATEWAY_TOKENS', generated: false };
  }

  try {
    const onDisk = JSON.parse(await fs.readFile(TOKEN_FILE, 'utf8'));
    tokens = new Map(Object.entries(onDisk));
    if (tokens.size) return { source: TOKEN_FILE, generated: false };
  } catch {
    // No file yet — fall through and make one.
  }

  const made = {};
  for (const mspId of mspIds) made[crypto.randomBytes(24).toString('base64url')] = mspId;
  tokens = new Map(Object.entries(made));
  // 0600: the file is a credential, not configuration.
  await fs.writeFile(TOKEN_FILE, JSON.stringify(made, null, 2), { mode: 0o600 });
  return { source: TOKEN_FILE, generated: true };
}

/** The MSP this token may act as, or null. */
export function mspForToken(token) {
  if (!token) return null;
  return tokens.get(token) ?? null;
}

export function tokenMap() {
  return Object.fromEntries(tokens);
}

/** Bearer token from the header, or the `token` query parameter for EventSource,
 *  which cannot set headers. */
export function tokenFromRequest(req, url) {
  const header = req.headers.authorization;
  if (header?.startsWith('Bearer ')) return header.slice(7).trim();
  return url?.searchParams.get('token') ?? null;
}

const LOOPBACK = new Set(['127.0.0.1', '::1', '::ffff:127.0.0.1']);

/**
 * `/api/dev/tokens` hands the browser its credentials so the demo is one click.
 * That is only acceptable when BOTH hold:
 *   - the operator asked for it (GATEWAY_DEV=1), and
 *   - the request came from this machine.
 * Either missing, and the endpoint does not exist.
 */
export function devTokensAllowed(req) {
  return DEV_MODE && LOOPBACK.has(req.socket.remoteAddress ?? '');
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// A fixed window per token. Crude, but it turns "hammer the ledger until MVCC
// conflicts jam it" into a bounded nuisance, which is the threat that matters
// here — the ledger rejects concurrent writes to the same key, so a flood is a
// denial of service against the corridor rather than just against this box.
const WINDOW_MS = 10_000;
const MAX_IN_WINDOW = Number(process.env.GATEWAY_RATE_LIMIT ?? 60);
const buckets = new Map();

export function rateLimit(key) {
  const now = Date.now();
  const bucket = buckets.get(key);
  if (!bucket || now >= bucket.resetAt) {
    buckets.set(key, { count: 1, resetAt: now + WINDOW_MS });
    return { ok: true, remaining: MAX_IN_WINDOW - 1, retryAfter: 0 };
  }
  if (bucket.count >= MAX_IN_WINDOW) {
    return { ok: false, remaining: 0, retryAfter: Math.ceil((bucket.resetAt - now) / 1000) };
  }
  bucket.count += 1;
  return { ok: true, remaining: MAX_IN_WINDOW - bucket.count, retryAfter: 0 };
}

// Buckets are only ever read by the token that created them, so stale ones are
// just memory. Sweep occasionally rather than tracking every expiry.
setInterval(() => {
  const now = Date.now();
  for (const [key, b] of buckets) if (now >= b.resetAt) buckets.delete(key);
}, 60_000).unref();
