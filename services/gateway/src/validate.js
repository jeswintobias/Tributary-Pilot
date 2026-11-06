// Request validation.
//
// The chaincode enforces all of this too, and the chaincode is the real trust
// boundary — the CLI talks to the peer directly and never passes through here.
// This layer exists to fail fast and cheaply, so a malformed request costs a
// string comparison rather than a round trip to three peers.
//
// Anything that reaches the ledger is still checked there. Never the other way
// round.

const ID = /^[A-Za-z0-9_.:-]{1,64}$/;
const PAYEE_REF = /^[A-Za-z0-9_.:-]{1,128}$/;
const CCY = /^[A-Z]{3}$/;
const DIGITS = /^[0-9]{1,20}$/;

export class BadRequest extends Error {
  constructor(message) {
    super(message);
    this.name = 'BadRequest';
  }
}

const str = (v) => (typeof v === 'string' ? v : v == null ? '' : String(v));

function id(name, value, re = ID) {
  const v = str(value);
  if (!re.test(v)) {
    throw new BadRequest(
      `${name} must be 1–${re === PAYEE_REF ? 128 : 64} characters of letters, digits or - _ . :`);
  }
  return v;
}

function ccy(name, value) {
  const v = str(value).toUpperCase();
  if (!CCY.test(v)) throw new BadRequest(`${name} must be a three-letter code such as AED or INR`);
  return v;
}

// Amounts travel as decimal strings, never as JavaScript numbers: a uint64 in
// minor units exceeds what a double can hold exactly, and silently rounding
// someone's money is the exact failure this project refuses to have.
function amount(name, value) {
  const v = str(value).trim();
  if (!DIGITS.test(v)) throw new BadRequest(`${name} must be a whole number of minor units`);
  const asBig = BigInt(v);
  if (asBig <= 0n) throw new BadRequest(`${name} must be greater than zero`);
  if (asBig > (2n ** 64n - 1n)) throw new BadRequest(`${name} is larger than the ledger can hold`);
  return v;
}

/** Seconds since the epoch, inside the contract's settlement window. */
function expiry(value) {
  const v = str(value).trim();
  if (!DIGITS.test(v)) throw new BadRequest('expiresAt must be a Unix timestamp in seconds');
  const at = Number(v);
  const now = Math.floor(Date.now() / 1000);
  if (at <= now) throw new BadRequest('expiresAt is already in the past');
  // The contract caps this at 900s; rejecting here gives a clearer message than
  // an endorsement failure would.
  if (at > now + 900) throw new BadRequest('expiresAt is more than 900 seconds ahead');
  return v;
}

export function validateCreateTransfer(body) {
  return {
    transferID: id('transferID', body.transferID),
    senderLegAmount: amount('senderLegAmount', body.senderLegAmount),
    senderCcy: ccy('senderCcy', body.senderCcy),
    payoutLegAmount: amount('payoutLegAmount', body.payoutLegAmount),
    payoutCcy: ccy('payoutCcy', body.payoutCcy),
    quoteID: id('quoteID', body.quoteID),
    expiresAt: expiry(body.expiresAt),
    payoutOrg: id('payoutOrg', body.payoutOrg),
    payoutPayeeRef: id('payoutPayeeRef', body.payoutPayeeRef, PAYEE_REF),
  };
}

export function validateSeed(body) {
  return { ccy: ccy('ccy', body.ccy), amount: amount('amount', body.amount) };
}

export function validateTransferId(value) {
  return id('transferID', value);
}

/** Guard against an oversized body before it is parsed. */
export const MAX_BODY_BYTES = 16 * 1024;
