import { test } from 'node:test';
import assert from 'node:assert/strict';

import { toDecimal, toMinorUnits, fractionDigits } from '../src/amount.js';
import { rateToDecimal, terminates } from '../src/rate.js';
import { esc } from '../src/xml.js';
import { pacs008, pacs002, pacs004, messagesFor, uetrFor, STATUS_FOR_STATE } from '../src/messages.js';

const settled = {
  transferID: 'TP-UAE-IN-0001',
  state: 'SETTLED',
  senderOrg: 'Org1MSP',
  senderLegAmount: '220000',
  senderCcy: 'AED',
  payoutOrg: 'Org2MSP',
  payoutLegAmount: '5003482',
  payoutCcy: 'INR',
  payoutPayeeRef: 'sha256:collegefees',
  quote: { quoteID: 'q-1', oracleID: 'tributary-fx-1', rateNum: 227431, rateDen: 10000 },
  createdAt: 1794000000,
  preparedAt: 1794000100,
  resolvedAt: 1794000200,
};

// ---------------------------------------------------------------------------
// Amounts
// ---------------------------------------------------------------------------

test('minor units become the currency decimal exactly', () => {
  assert.equal(toDecimal('5003482', 'INR'), '50034.82');
  assert.equal(toDecimal('220000', 'AED'), '2200.00');
  assert.equal(toDecimal('1', 'AED'), '0.01');
  assert.equal(toDecimal('0', 'AED'), '0.00');
});

// JPY has no minor unit and KWD has three. Dividing by 100 is wrong for both,
// in opposite directions, and silently.
test('currencies that are not two-decimal are handled, not assumed', () => {
  assert.equal(fractionDigits('JPY'), 0);
  assert.equal(fractionDigits('KWD'), 3);
  assert.equal(toDecimal('5000', 'JPY'), '5000');
  assert.equal(toDecimal('5000', 'KWD'), '5.000');
  assert.throws(() => fractionDigits('ZZZ'), /refusing to guess/);
});

test('an amount beyond 2^53 converts exactly', () => {
  const big = '9007199254740993';
  const decimal = toDecimal(big, 'INR');
  assert.equal(decimal, '90071992547409.93');
  assert.equal(toMinorUnits(decimal, 'INR'), big);
  // The obvious implementation gets this wrong, which is why the test names it.
  assert.notEqual(decimal, (Number(big) / 100).toFixed(2));
});

test('a number is refused, because it may already have lost precision', () => {
  assert.throws(() => toDecimal(5003482, 'INR'), /string or BigInt/);
});

test('reading a decimal back refuses to round', () => {
  assert.throws(() => toMinorUnits('1.234', 'INR'), /rounding it here/);
  assert.equal(toMinorUnits('1.23', 'INR'), '123');
  assert.equal(toMinorUnits('5000', 'JPY'), '5000');
});

// ---------------------------------------------------------------------------
// The rate
// ---------------------------------------------------------------------------

test('a rational that terminates is written exactly', () => {
  const r = rateToDecimal(227431, 10000);
  assert.equal(r.value, '22.7431');
  assert.equal(r.exact, true);
});

// ISO wants a decimal, and not every rate has one. The mapping must be able to
// say so rather than quietly shipping an approximation as if it were the rate.
test('a rational that does not terminate is flagged, not disguised', () => {
  const r = rateToDecimal(1, 3);
  assert.equal(r.exact, false);
  assert.match(r.value, /^0\.3{5,}$/);
  assert.equal(terminates(1, 3), false);
  assert.equal(terminates(1, 8), true);
  // 3/6 reduces to 1/2, which does terminate.
  assert.equal(terminates(3, 6), true);
});

// ---------------------------------------------------------------------------
// XML
// ---------------------------------------------------------------------------

test('a reference with XML metacharacters cannot break the document', () => {
  assert.equal(esc('a&b<c>"d\'e'), 'a&amp;b&lt;c&gt;&quot;d&apos;e');
  const hostile = { ...settled, payoutPayeeRef: 'sha256:<Cdtr>&evil' };
  const { xml } = pacs008(hostile);
  assert.ok(!xml.includes('<Cdtr>&evil'), 'the raw reference reached the document');
  assert.ok(xml.includes('&lt;Cdtr&gt;&amp;evil'));
});

// ---------------------------------------------------------------------------
// pacs.008
// ---------------------------------------------------------------------------

test('the instruction carries both legs, the rate and both institutions', () => {
  const { xml, uetr, rateExact } = pacs008(settled);
  assert.ok(xml.startsWith('<?xml version="1.0" encoding="UTF-8"?>'));
  assert.ok(xml.includes('urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08'));
  // The settlement amount is the payout leg: what the beneficiary's bank delivers.
  assert.ok(xml.includes('<IntrBkSttlmAmt Ccy="INR">50034.82</IntrBkSttlmAmt>'));
  // The instructed amount is what the sender was told to pay.
  assert.ok(xml.includes('<InstdAmt Ccy="AED">2200.00</InstdAmt>'));
  assert.ok(xml.includes('<XchgRate>22.7431</XchgRate>'));
  assert.ok(xml.includes('<Id>Org1MSP</Id>'));
  assert.ok(xml.includes('<Id>Org2MSP</Id>'));
  assert.equal(rateExact, true);
  assert.match(uetr, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
});

// The whole point of the salted reference is that it is the only thing about a
// person on the ledger. A mapping that invented a name field would undo it.
test('no personal data field is emitted, because there is none to emit', () => {
  const { xml } = pacs008(settled);
  for (const forbidden of ['<Nm>', '<PstlAdr>', '<BirthDt>', '<CtryOfBirth>', '<EmailAdr>']) {
    assert.ok(!xml.includes(forbidden), `the message carries ${forbidden}`);
  }
  assert.ok(xml.includes('sha256:collegefees'), 'the opaque reference should still identify the payee');
  assert.ok(xml.includes('TRIBUTARY-SALTED-REF'), 'the scheme should say what kind of identifier it is');
});

test('the tracker is stable for a transfer, not random per call', () => {
  assert.equal(uetrFor('TP-1'), uetrFor('TP-1'));
  assert.notEqual(uetrFor('TP-1'), uetrFor('TP-2'));
});

// ---------------------------------------------------------------------------
// pacs.002
// ---------------------------------------------------------------------------

test('every ledger state maps to a status', () => {
  for (const state of ['CAPTURED', 'ACCEPTED', 'PREPARED', 'SETTLED', 'ABORTED']) {
    const { status, xml } = pacs002({ ...settled, state });
    assert.ok(status, `no status for ${state}`);
    assert.ok(xml.includes(`<TxSts>${status}</TxSts>`));
  }
  assert.equal(STATUS_FOR_STATE.SETTLED, 'ACSC');
  // PREPARED has no exact counterpart in the standard; PDNG understates what
  // the ledger knows, and that is recorded rather than hidden.
  assert.equal(STATUS_FOR_STATE.PREPARED, 'PDNG');
});

test('an unmapped state is refused rather than guessed at', () => {
  assert.throws(() => pacs002({ ...settled, state: 'WHATEVER' }), /no ISO status mapped/);
});

test('a rejection carries a reason', () => {
  const { xml } = pacs002({ ...settled, state: 'ABORTED' }, { reason: 'sanctions match on the beneficiary' });
  assert.ok(xml.includes('<TxSts>RJCT</TxSts>'));
  assert.ok(xml.includes('sanctions match on the beneficiary'));
});

// ---------------------------------------------------------------------------
// pacs.004
// ---------------------------------------------------------------------------

test('an aborted transfer that reserved value produces a return', () => {
  const aborted = { ...settled, state: 'ABORTED' };
  const { xml } = pacs004(aborted);
  assert.ok(xml.includes('urn:iso:std:iso:20022:tech:xsd:pacs.004.001.09'));
  assert.ok(xml.includes('<RtrdIntrBkSttlmAmt Ccy="INR">50034.82</RtrdIntrBkSttlmAmt>'));
});

test('a settled transfer is not a return', () => {
  assert.throws(() => pacs004(settled), /describes a return/);
});

// ---------------------------------------------------------------------------
// The set
// ---------------------------------------------------------------------------

test('a settled transfer yields the instruction and its completion', () => {
  const kinds = messagesFor(settled).map((m) => m.kind);
  assert.deepEqual(kinds, ['pacs.008', 'pacs.002']);
});

test('an abort after reserving also yields a return', () => {
  const kinds = messagesFor({ ...settled, state: 'ABORTED' }).map((m) => m.kind);
  assert.deepEqual(kinds, ['pacs.008', 'pacs.002', 'pacs.004']);
});

// A transfer aborted before Prepare moved nothing, so there is nothing to
// return — only a rejection.
test('an abort before reserving yields no return', () => {
  const kinds = messagesFor({ ...settled, state: 'ABORTED', preparedAt: 0 }).map((m) => m.kind);
  assert.deepEqual(kinds, ['pacs.008', 'pacs.002']);
});
