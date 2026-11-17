import { test } from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { quoteBytes, signQuote, verifyQuote, applyRate } from '../src/quote.js';
import { rateFor, knownPairs } from '../src/rates.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const VECTOR = path.join(HERE, '..', '..', '..', 'chaincode', 'settlement', 'settlement',
  'testdata', 'quote-vector.json');

function keys() {
  const { privateKey, publicKey } = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  return {
    privateKeyPem: privateKey.export({ type: 'pkcs8', format: 'pem' }),
    publicKeyPem: publicKey.export({ type: 'spki', format: 'pem' }),
  };
}

const sample = {
  quoteID: 'q-1', oracleID: 'o-1', baseCcy: 'AED', quoteCcy: 'INR',
  rateNum: 227431, rateDen: 10000, expiresAt: 1794000000, maxSlippageBps: 25,
};

// ---------------------------------------------------------------------------

test('a signed quote verifies', () => {
  const { privateKeyPem, publicKeyPem } = keys();
  const sig = signQuote(sample, privateKeyPem);
  assert.ok(verifyQuote(sample, sig, publicKeyPem));
});

test('a quote altered after signing does not verify', () => {
  const { privateKeyPem, publicKeyPem } = keys();
  const sig = signQuote(sample, privateKeyPem);
  assert.ok(!verifyQuote({ ...sample, rateNum: 227432 }, sig, publicKeyPem),
    'moving the rate by one must break the signature');
  assert.ok(!verifyQuote({ ...sample, expiresAt: sample.expiresAt + 1 }, sig, publicKeyPem),
    'extending the expiry must break the signature');
});

test('another key cannot forge a quote', () => {
  const mine = keys();
  const theirs = keys();
  const sig = signQuote(sample, theirs.privateKeyPem);
  assert.ok(!verifyQuote(sample, sig, mine.publicKeyPem));
});

// Without length prefixes these two serialise identically and a signature over
// one verifies the other.
test('adjacent fields cannot bleed into each other', () => {
  const a = { ...sample, quoteID: 'AB', oracleID: 'C' };
  const b = { ...sample, quoteID: 'A', oracleID: 'BC' };
  assert.notEqual(quoteBytes(a).toString('utf8'), quoteBytes(b).toString('utf8'));
});

test('the encoding is stable', () => {
  const first = quoteBytes(sample).toString('base64');
  for (let i = 0; i < 50; i += 1) {
    assert.equal(quoteBytes(sample).toString('base64'), first);
  }
});

test('byte length is used, not character count', () => {
  // A field with a multi-byte character must be prefixed with its BYTE
  // length, because Go measures len(string) in bytes. Getting this wrong
  // would only show up on a non-ASCII quote ID, which is exactly the kind of
  // bug that reaches production.
  const q = { ...sample, quoteID: 'é' };
  const text = quoteBytes(q).toString('utf8');
  assert.ok(text.includes('2:é'), `expected a 2-byte prefix, got ${text}`);
});

// ---------------------------------------------------------------------------

test('the committed cross-language vector still verifies', () => {
  const vector = JSON.parse(readFileSync(VECTOR, 'utf8'));
  assert.equal(
    quoteBytes(vector.quote).toString('base64'),
    vector.canonicalBase64,
    'this service no longer produces the bytes in the vector the chaincode checks against',
  );
  assert.ok(verifyQuote(vector.quote, vector.signatureBase64, vector.publicKeyPEM));
});

// ---------------------------------------------------------------------------

test('rates are exact rationals, never decimals', () => {
  const { rateNum, rateDen } = rateFor('AED', 'INR');
  assert.equal(rateNum, '227431');
  assert.equal(rateDen, '10000');
  for (const pair of knownPairs()) {
    const [base, quote] = pair.split('/');
    const r = rateFor(base, quote);
    assert.match(r.rateNum, /^\d+$/);
    assert.match(r.rateDen, /^[1-9]\d*$/);
  }
});

test('an unknown pair is refused rather than guessed', () => {
  assert.throws(() => rateFor('XXX', 'YYY'), /no rate for XXX\/YYY/);
});

test('drift moves the rate deterministically', () => {
  const base = rateFor('AED', 'INR');
  const up = rateFor('AED', 'INR', { driftBps: 100 });
  assert.notEqual(up.rateNum, base.rateNum);
  assert.equal(up.rateNum, rateFor('AED', 'INR', { driftBps: 100 }).rateNum,
    'the same drift must give the same rate, or no test of the arithmetic is possible');
});

test('applying a rate uses integer arithmetic throughout', () => {
  assert.equal(applyRate('220000', 227431, 10000), '5003482');
  // Beyond 2^53. A double would round this; BigInt does not.
  const huge = '9007199254740993';
  assert.equal(applyRate(huge, 1, 1), huge);
  assert.notEqual(applyRate(huge, 1, 1), String(Number(huge)));
});

test('a zero denominator is refused', () => {
  assert.throws(() => applyRate('100', 1, 0), /denominator is zero/);
});
