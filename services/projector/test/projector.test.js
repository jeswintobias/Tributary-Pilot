// What a projector has to get right is not "does it write the row". It is what
// happens when the process dies between two writes, when the same event
// arrives twice, and when one job can never succeed. These tests are about
// those.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { openDb, readCheckpoint } from '../src/db.js';
import { applyEvent, payoutKey, PAYOUT_JOB } from '../src/projector.js';
import { claim, complete, fail, enqueue, queueDepth, backoffMs } from '../src/queue.js';
import { parsePayload } from '../src/events.js';

const db = () => openDb(':memory:');

/** A chaincode event as the Fabric SDK hands it over: payload still text. */
function settlementEvent(name, fields, { block = 10, tx = 'tx-1' } = {}) {
  const payload = {
    transferID: 'TP-1', state: name.replace('Transfer', '').toUpperCase(),
    senderOrg: 'Org1MSP', senderLegAmount: 220000, senderCcy: 'AED',
    payoutOrg: 'Org2MSP', payoutLegAmount: 3000000, payoutCcy: 'INR',
    payoutPayeeRef: 'sha256:payee', proofHash: '', at: 1700000000,
    ...fields,
  };
  return { name, blockNumber: block, transactionId: tx, payloadText: JSON.stringify(payload) };
}

// ---------------------------------------------------------------------------
// Applying events
// ---------------------------------------------------------------------------

test('an event applied twice changes the read model once', () => {
  const d = db();
  const ev = settlementEvent('TransferCaptured', {});

  assert.equal(applyEvent(d, ev), 'applied');
  assert.equal(applyEvent(d, ev), 'skipped');

  const rows = d.prepare('SELECT * FROM transfers').all();
  assert.equal(rows.length, 1);
  assert.equal(rows[0].state, 'CAPTURED');
  const cp = readCheckpoint(d);
  assert.equal(cp.events_applied, 1, 'the replay must not count as applied');
  assert.equal(cp.events_skipped, 1);
});

test('the checkpoint resumes from the last applied block, not the one after', () => {
  const d = db();
  // Two events in one block is the case that makes this matter.
  applyEvent(d, settlementEvent('TransferCaptured', { transferID: 'A' }, { block: 42, tx: 'tx-a' }));
  assert.equal(readCheckpoint(d).next_block, 42);

  applyEvent(d, settlementEvent('TransferCaptured', { transferID: 'B' }, { block: 42, tx: 'tx-b' }));
  assert.equal(readCheckpoint(d).next_block, 42,
    'resuming from 43 would lose the second event in block 42 after a crash');
});

test('replaying a whole block after a restart loses nothing and duplicates nothing', () => {
  const d = db();
  const block = [
    settlementEvent('TransferCaptured', { transferID: 'A' }, { block: 7, tx: 'tx-a' }),
    settlementEvent('TransferCaptured', { transferID: 'B' }, { block: 7, tx: 'tx-b' }),
    settlementEvent('TransferCaptured', { transferID: 'C' }, { block: 7, tx: 'tx-c' }),
  ];
  // The process applies the first two and dies.
  applyEvent(d, block[0]);
  applyEvent(d, block[1]);

  // It comes back and Fabric re-delivers the block from the checkpoint.
  const outcomes = block.map((ev) => applyEvent(d, ev));
  assert.deepEqual(outcomes, ['skipped', 'skipped', 'applied']);

  const ids = d.prepare('SELECT transfer_id FROM transfers ORDER BY transfer_id').all();
  assert.deepEqual(ids.map((r) => r.transfer_id), ['A', 'B', 'C']);
});

test('the read model never goes backwards', () => {
  const d = db();
  applyEvent(d, settlementEvent('TransferCaptured', {}, { block: 1, tx: 'tx-1' }));
  applyEvent(d, settlementEvent('TransferSettled', {}, { block: 4, tx: 'tx-4' }));
  assert.equal(d.prepare('SELECT state FROM transfers').get().state, 'SETTLED');

  // A CAPTURED arriving late, from a different transaction so dedupe does not
  // catch it. A settled transfer must not revert.
  applyEvent(d, settlementEvent('TransferCaptured', {}, { block: 5, tx: 'tx-late' }));
  assert.equal(d.prepare('SELECT state FROM transfers').get().state, 'SETTLED');
});

test('an unknown event advances the checkpoint instead of halting the projector', () => {
  const d = db();
  const ev = {
    name: 'SomethingAddedLater', blockNumber: 9, transactionId: 'tx-9',
    payloadText: JSON.stringify({ whatever: true }),
  };
  assert.equal(applyEvent(d, ev), 'skipped');
  assert.equal(readCheckpoint(d).next_block, 9,
    'an unrecognised event must not wedge the stream for ever');
});

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

test('an amount beyond 2^53 survives the projection exactly', () => {
  const d = db();
  // 9007199254740993 is the first integer a double cannot represent.
  const big = '9007199254740993';
  const ev = {
    name: 'TransferCaptured', blockNumber: 1, transactionId: 'tx-big',
    payloadText: `{"transferID":"BIG","state":"CAPTURED","senderOrg":"Org1MSP",`
      + `"senderLegAmount":${big},"senderCcy":"AED","payoutOrg":"Org2MSP",`
      + `"payoutLegAmount":1,"payoutCcy":"INR","payoutPayeeRef":"p","at":1}`,
  };
  applyEvent(d, ev);

  const stored = d.prepare('SELECT sender_amount FROM transfers WHERE transfer_id = ?').get('BIG');
  assert.equal(stored.sender_amount, big);
  assert.notEqual(stored.sender_amount, String(JSON.parse(ev.payloadText).senderLegAmount),
    'the naive parse is the thing this is guarding against, so it must differ');
});

test('parsePayload keeps integers exact and leaves other values alone', () => {
  const p = parsePayload('{"n":9007199254740993,"s":"text","b":true,"f":1.5,"z":null}');
  assert.equal(p.n, '9007199254740993');
  assert.equal(p.s, 'text');
  assert.equal(p.b, true);
  assert.equal(p.f, 1.5);
  assert.equal(p.z, null);
});

// ---------------------------------------------------------------------------
// The outbox
// ---------------------------------------------------------------------------

test('a settled transfer enqueues exactly one payout, however many times it is seen', () => {
  const d = db();
  const ev = settlementEvent('TransferSettled', {}, { block: 3, tx: 'tx-settle' });
  applyEvent(d, ev);
  applyEvent(d, ev);
  // The same settlement re-delivered in a different transaction, which dedupe
  // on (block, tx, event) would NOT catch.
  applyEvent(d, settlementEvent('TransferSettled', {}, { block: 3, tx: 'tx-other' }));

  const jobs = d.prepare('SELECT * FROM outbox').all();
  assert.equal(jobs.length, 1, 'the dedupe key on the job is the second line of defence');
  assert.equal(jobs[0].kind, PAYOUT_JOB);
  assert.equal(jobs[0].dedupe_key, payoutKey('TP-1'));
});

test('a capture or an abort enqueues nothing', () => {
  const d = db();
  applyEvent(d, settlementEvent('TransferCaptured', {}, { block: 1, tx: 'a' }));
  applyEvent(d, settlementEvent('TransferAborted', {}, { block: 2, tx: 'b' }));
  assert.equal(d.prepare('SELECT COUNT(*) AS n FROM outbox').get().n, 0);
});

test('a failure part-way through an event leaves no trace of it at all', () => {
  const d = db();
  // Force the enqueue to fail after the transfer row has been written, which
  // is the exact crash the single transaction exists to survive.
  const ev = settlementEvent('TransferSettled', { payoutLegAmount: 'not-a-number' },
    { block: 5, tx: 'tx-bad' });

  assert.throws(() => applyEvent(d, ev));

  assert.equal(d.prepare('SELECT COUNT(*) AS n FROM transfers').get().n, 0,
    'the read model write must have rolled back with everything else');
  assert.equal(d.prepare('SELECT COUNT(*) AS n FROM outbox').get().n, 0);
  assert.equal(d.prepare('SELECT COUNT(*) AS n FROM applied_events').get().n, 0,
    'marking it applied would lose the event for ever once the bug is fixed');
  assert.equal(readCheckpoint(d).next_block, 0,
    'the checkpoint must not move past an event that was not applied');
});

// ---------------------------------------------------------------------------
// Windows and obligations
// ---------------------------------------------------------------------------

test('netting events build the window read model', () => {
  const d = db();
  applyEvent(d, {
    name: 'ObligationSubmitted', blockNumber: 2, transactionId: 'tx-ob',
    payloadText: JSON.stringify({
      obligationID: 'OB-1', seq: 29909256, debtor: 'Org1MSP', creditor: 'Org2MSP',
      ccy: 'AED', amount: 400000, at: 1700000000,
    }),
  });
  applyEvent(d, {
    name: 'WindowSettled', blockNumber: 3, transactionId: 'tx-win',
    payloadText: JSON.stringify({
      ccy: 'AED', seq: 29909256, state: 'SETTLED', obligations: 6,
      grossValue: 1800000, netValue: 100000, postings: 1,
      compressionBps: 9444, closedBy: 'Org1MSP', closedAt: 1700000060,
    }),
  });

  const w = d.prepare('SELECT * FROM windows').get();
  assert.equal(w.gross, '1800000');
  assert.equal(w.net, '100000');
  assert.equal(w.postings, 1);
  assert.equal(w.compression_bps, 9444);
  const ob = d.prepare('SELECT * FROM obligations').get();
  assert.equal(ob.amount, '400000');
  assert.equal(ob.seq, 29909256);
});

// ---------------------------------------------------------------------------
// The queue itself
// ---------------------------------------------------------------------------

test('a job is claimed once, not twice', () => {
  const d = db();
  enqueue(d, { kind: 'payout', dedupeKey: 'k1', payload: { transferID: 'A' } });

  const first = claim(d, 'worker-a');
  const second = claim(d, 'worker-b');
  assert.ok(first);
  assert.equal(second, null, 'a leased job must not be handed out again');
});

test('an abandoned lease is picked up by another worker', () => {
  const d = db();
  enqueue(d, { kind: 'payout', dedupeKey: 'k1', payload: { transferID: 'A' } });
  const now = Date.now();

  const taken = claim(d, 'worker-a', { now, leaseMs: 1000 });
  assert.ok(taken);
  assert.equal(claim(d, 'worker-b', { now: now + 500 }), null);

  const reclaimed = claim(d, 'worker-b', { now: now + 1500 });
  assert.ok(reclaimed, 'a worker that died must not hold its job for ever');
  assert.equal(reclaimed.id, taken.id);
});

test('backoff grows and then stops growing', () => {
  assert.deepEqual([1, 2, 3, 4].map(backoffMs), [1000, 2000, 4000, 8000]);
  assert.equal(backoffMs(99), 60_000, 'an unbounded backoff is a job that never runs again');
});

test('a failed job comes back later, not immediately', () => {
  const d = db();
  enqueue(d, { kind: 'payout', dedupeKey: 'k1', payload: { transferID: 'A' } });
  const now = Date.now();
  const job = claim(d, 'w', { now });

  fail(d, job, new Error('rail down'), { now });

  assert.equal(claim(d, 'w', { now: now + 100 }), null, 'retrying at once is not a retry');
  assert.ok(claim(d, 'w', { now: now + 1500 }));
});

test('completing a job and recording its result are one step', () => {
  const d = db();
  enqueue(d, { kind: 'payout', dedupeKey: 'k1', payload: { transferID: 'A' } });
  const job = claim(d, 'w');

  assert.throws(() => complete(d, job, () => { throw new Error('write failed'); }));

  assert.equal(d.prepare('SELECT COUNT(*) AS n FROM outbox').get().n, 1,
    'a job whose result could not be recorded must stay queued');
});

test('queueDepth distinguishes ready work from leased work', () => {
  const d = db();
  const now = Date.now();
  // availableAt is passed rather than defaulted: enqueue would call Date.now()
  // again, and a clock tick between the two would make these jobs due one
  // millisecond after the instant the depth is measured at. That is how this
  // test failed the first time it ran.
  enqueue(d, { kind: 'payout', dedupeKey: 'a', payload: {}, availableAt: now });
  enqueue(d, { kind: 'payout', dedupeKey: 'b', payload: {}, availableAt: now });
  claim(d, 'w', { now, leaseMs: 10_000 });

  const depth = queueDepth(d, now);
  assert.equal(depth.queued, 2);
  assert.equal(depth.ready, 1);
  assert.equal(depth.leased, 1);
  assert.equal(depth.dead, 0);
});
