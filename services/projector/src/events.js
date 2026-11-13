// Parsing chaincode event payloads without rounding anybody's money.
//
// The chaincode marshals amounts as JSON numbers, because that is what Go's
// encoding/json does with a uint64. JSON.parse turns those into doubles, and a
// double holds integers exactly only up to 2^53 - 1. A uint64 in minor units
// goes far beyond that, so the obvious parse silently corrupts large amounts:
//
//   JSON.parse('{"a":9007199254740993}').a  ->  9007199254740992
//
// No error, no warning, one fil gone. The project's rule is that money is
// never a float, and the gateway already honours it on the way IN by parsing
// request amounts with BigInt — but it was being undone on the way OUT. This
// is where that is fixed for the read model.
//
// The reviver's third argument carries `source`, the original text of the
// primitive before it was converted. Keeping that text for every integer means
// the value never passes through a double at all. It is stored as TEXT and
// compared with BigInt.

/** Field names whose values are money and must stay exact. */
const MONEY_FIELDS = new Set([
  'senderLegAmount', 'payoutLegAmount', 'amount',
  'grossValue', 'netValue', 'debits', 'credits', 'balance', 'outstanding',
]);

/**
 * Parse a chaincode event payload, keeping integers as their exact decimal
 * text and leaving everything else alone.
 *
 * Every integer is kept as text, not only the money fields: deciding by field
 * name would quietly reintroduce the bug for the next field someone adds. The
 * callers that want a number for a small value — a block, a sequence, a
 * timestamp — convert it explicitly, which is the point at which a precision
 * question becomes visible.
 */
export function parsePayload(text) {
  return JSON.parse(text, function (key, value, context) {
    if (typeof value === 'number' && Number.isInteger(value) && context?.source !== undefined) {
      return context.source;
    }
    return value;
  });
}

/** A field that is known to be money, as exact text. Missing reads as "0". */
export function money(payload, field) {
  const v = payload?.[field];
  if (v === undefined || v === null) return '0';
  const text = String(v);
  if (!/^-?\d+$/.test(text)) {
    throw new Error(`${field} is ${JSON.stringify(v)}, which is not an integer amount`);
  }
  return text;
}

/** A field that is small enough to be a JavaScript number without question. */
export function count(payload, field) {
  const v = payload?.[field];
  if (v === undefined || v === null) return 0;
  const n = Number(v);
  if (!Number.isSafeInteger(n)) {
    throw new Error(`${field} is ${JSON.stringify(v)}, which is not a safe integer`);
  }
  return n;
}

export function isMoneyField(field) {
  return MONEY_FIELDS.has(field);
}

/** Event names the projector knows how to apply. */
export const TRANSFER_EVENTS = new Set([
  'TransferCaptured', 'TransferAccepted', 'TransferPrepared',
  'TransferSettled', 'TransferAborted',
]);
export const EVENT_OBLIGATION = 'ObligationSubmitted';
export const EVENT_WINDOW_SETTLED = 'WindowSettled';
