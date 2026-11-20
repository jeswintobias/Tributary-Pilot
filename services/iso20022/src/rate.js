// The exchange rate, as ISO 20022 insists on writing it.
//
// The ledger holds a rate as an exact rational: 227431/10000. ISO 20022's
// XchgRate is a decimal. For this corridor the two agree exactly, because the
// denominator is a power of ten — but that is a property of this rate, not of
// rates in general, and a mapping that assumed it would silently round
// somebody's money the first time a rate came from a source that quotes in
// thirds.
//
// So: emit the exact decimal when the rational has one, and refuse to pretend
// otherwise when it does not. A rational terminates in decimal exactly when
// its reduced denominator has no prime factors besides 2 and 5.

function gcd(a, b) {
  while (b) { [a, b] = [b, a % b]; }
  return a;
}

/** Does num/den have a terminating decimal expansion? */
export function terminates(num, den) {
  let d = BigInt(den) / gcd(BigInt(num), BigInt(den));
  while (d % 2n === 0n) d /= 2n;
  while (d % 5n === 0n) d /= 5n;
  return d === 1n;
}

/**
 * The rate as a decimal string.
 *
 * Exact when the rational terminates. When it does not, the result is rounded
 * to `maxScale` digits and `exact` is false — so a caller can refuse to emit
 * a message rather than quietly ship an approximation.
 */
export function rateToDecimal(rateNum, rateDen, { maxScale = 10 } = {}) {
  const num = BigInt(rateNum);
  const den = BigInt(rateDen);
  if (den === 0n) throw new Error('rate denominator is zero');
  if (num < 0n || den < 0n) throw new Error('a negative exchange rate is not a rate');

  const exact = terminates(num, den);

  // Long division to maxScale digits, entirely in BigInt.
  const scale = BigInt(maxScale);
  const factor = 10n ** scale;
  const scaled = (num * factor) / den;
  const whole = scaled / factor;
  let fraction = (scaled % factor).toString().padStart(maxScale, '0');
  fraction = fraction.replace(/0+$/, '');

  return {
    value: fraction ? `${whole}.${fraction}` : whole.toString(),
    exact,
  };
}
