// Minor units to an ISO 20022 decimal, without a float anywhere.
//
// The ledger holds money as an integer count of minor units — 5003482 paise.
// ISO 20022 writes it as a decimal with the currency's own number of fraction
// digits — 50034.82. The conversion is where a project that has been careful
// about integers for ten thousand lines usually stops being careful, because
// `amount / 100` is right there and looks fine.
//
// It is not fine. A uint64 of minor units exceeds what a double holds exactly,
// so dividing turns an exact figure into an approximate one at the last step
// before it leaves the system. This does the conversion as string surgery on
// the decimal digits, which is exact for any value the ledger can hold.

/**
 * Fraction digits per currency, as ISO 4217 defines them.
 *
 * Not every currency has two. JPY has none and a naive divide-by-100 would
 * report a hundredth of the real amount; KWD has three and the same code would
 * report ten times too much. The table is small because this corridor is
 * small; an unknown currency is refused rather than assumed.
 */
const FRACTION_DIGITS = {
  AED: 2, INR: 2, USD: 2, EUR: 2, GBP: 2,
  JPY: 0, KRW: 0,
  KWD: 3, BHD: 3, OMR: 3,
};

export function fractionDigits(ccy) {
  const digits = FRACTION_DIGITS[String(ccy).toUpperCase()];
  if (digits === undefined) {
    throw new Error(
      `no ISO 4217 fraction digits known for ${ccy}; refusing to guess, `
      + 'because guessing two is wrong for JPY and for KWD in opposite directions');
  }
  return digits;
}

/**
 * Format minor units as an ISO 20022 decimal string.
 *
 * `minor` is a decimal string or BigInt, never a number: accepting a number
 * would let a caller lose precision before this function ever saw the value.
 */
export function toDecimal(minor, ccy) {
  const text = typeof minor === 'bigint' ? minor.toString() : String(minor).trim();
  if (!/^-?\d+$/.test(text)) {
    throw new Error(`amount ${JSON.stringify(minor)} is not a whole number of minor units`);
  }
  if (typeof minor === 'number') {
    throw new Error('pass minor units as a string or BigInt; a number may already have lost precision');
  }

  const digits = fractionDigits(ccy);
  const negative = text.startsWith('-');
  const magnitude = negative ? text.slice(1) : text;

  if (digits === 0) return (negative ? '-' : '') + magnitude;

  const padded = magnitude.padStart(digits + 1, '0');
  const whole = padded.slice(0, padded.length - digits);
  const fraction = padded.slice(padded.length - digits);
  return `${negative ? '-' : ''}${whole}.${fraction}`;
}

/** The inverse, for reading a message back. Also exact. */
export function toMinorUnits(decimal, ccy) {
  const digits = fractionDigits(ccy);
  const text = String(decimal).trim();
  if (!/^-?\d+(\.\d+)?$/.test(text)) {
    throw new Error(`${JSON.stringify(decimal)} is not a decimal amount`);
  }
  const negative = text.startsWith('-');
  const [whole, fraction = ''] = (negative ? text.slice(1) : text).split('.');
  if (fraction.length > digits) {
    throw new Error(
      `${text} has ${fraction.length} fraction digits but ${ccy} has ${digits}: `
      + 'rounding it here would silently change the amount');
  }
  const combined = whole + fraction.padEnd(digits, '0');
  return (negative ? '-' : '') + (combined.replace(/^0+(?=\d)/, '') || '0');
}
