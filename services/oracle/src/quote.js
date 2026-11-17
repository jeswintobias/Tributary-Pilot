// The quote encoding, signing and verification.
//
// THIS FILE HAS A TWIN
//
// chaincode/settlement/settlement/quote.go implements the same encoding in Go,
// and the two must agree byte for byte or every quote this service signs will
// be rejected on chain with "not signed by oracle" and no clue as to why.
//
// Two implementations of one wire format is exactly where a mismatch hides, so
// it is pinned down rather than hoped for: `npm run vector` writes a signed
// quote and its canonical bytes into the chaincode's testdata, and a Go test
// re-derives those bytes and verifies that signature. If either side drifts,
// that test fails.
//
// WHY LENGTH PREFIXES
//
// Each field is written as <byte-length>:<field>. Without that, adjacent
// fields run together and two different quotes can produce identical bytes —
// a signature over one would verify the other. The domain separator at the
// front stops a signature over some other Tributary message being replayed as
// a quote.

import crypto from 'node:crypto';

const DOMAIN = 'tributary-fx-quote-v1';

/**
 * The exact bytes an oracle signs.
 *
 * Field order and the use of BYTE length — not character count — both matter:
 * Go measures len(string) in bytes, and a non-ASCII field would otherwise
 * encode differently on each side.
 */
export function quoteBytes(q) {
  const fields = [
    DOMAIN,
    q.quoteID,
    q.oracleID,
    q.baseCcy,
    q.quoteCcy,
    String(q.rateNum),
    String(q.rateDen),
    String(q.expiresAt),
    String(q.maxSlippageBps),
  ];
  const parts = [];
  for (const field of fields) {
    const value = Buffer.from(String(field), 'utf8');
    parts.push(Buffer.from(`${value.length}:`, 'utf8'), value);
  }
  return Buffer.concat(parts);
}

/** Sign a quote. Returns the base64 DER signature the chaincode expects. */
export function signQuote(q, privateKeyPem) {
  const signature = crypto.sign('sha256', quoteBytes(q), {
    key: privateKeyPem,
    dsaEncoding: 'der',
  });
  return signature.toString('base64');
}

/** Verify locally, so a bad signature is caught here rather than on chain. */
export function verifyQuote(q, signatureB64, publicKeyPem) {
  return crypto.verify('sha256', quoteBytes(q), {
    key: publicKeyPem,
    dsaEncoding: 'der',
  }, Buffer.from(signatureB64, 'base64'));
}

/**
 * The payout leg a quote implies for a given sender leg.
 *
 * Integer arithmetic with BigInt throughout. A rate is a rational, amounts are
 * minor units, and nothing here may become a double — the same rule the
 * chaincode follows, for the same reason.
 */
export function applyRate(senderAmount, rateNum, rateDen) {
  const amount = BigInt(senderAmount);
  const num = BigInt(rateNum);
  const den = BigInt(rateDen);
  if (den === 0n) throw new Error('rate denominator is zero');
  // Truncating division, matching Go's integer division so both sides compute
  // the same expected payout.
  return ((amount * num) / den).toString();
}
