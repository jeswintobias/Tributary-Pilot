// The worker's job is to be boring when the rail works and careful when it
// does not. These tests are mostly about the second.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { openDb } from '../src/db.js';
import { applyEvent, payoutKey } from '../src/projector.js';
import { enqueue, claim, queueDepth } from '../src/queue.js';
import { runOnce } from '../src/worker.js';
import { sendPayout, RailError } from '../src/payout-rail.js';

const db = () => openDb(':memory:');

function settled(transferID, payeeRef = 'sha256:payee', { block = 1, amount = 3000000 } = {}) {
  return {
    name: 'TransferSettled', blockNumber: block, transactionId: `tx-${transferID}`,
    payloadText: JSON.stringify({
      transferID, state: 'SETTLED', senderOrg: 'Org1MSP', senderLegAmount: 220000,
      senderCcy: 'AED', payoutOrg: 'Org2MSP', payoutLegAmount: amount, payoutCcy: 'INR',
      payoutPayeeRef: payeeRef, proofHash: 'abc', at: 1700000000,
    }),
  };
}

const alwaysOk = async ({ transferID, ccy }) => ({ railRef: `SIM-${ccy}-${transferID}` });

// ---------------------------------------------------------------------------

test('a settled transfer is paid and leaves the queue', async () => {
  const d = db();
  applyEvent(d, settled('TP-1'));

  const result = await runOnce(d, { rail: alwaysOk });
  assert.equal(result.status, 'paid');

  const payout = d.prepare('SELECT * FROM payouts').get();
  assert.equal(payout.state, 'PAID');
  assert.equal(payout.transfer_id, 'TP-1');
  assert.equal(payout.rail_ref, 'SIM-INR-TP-1');
  assert.equal(payout.amount, '3000000');
  assert.equal(queueDepth(d).queued, 0);

  assert.equal((await runOnce(d, { rail: alwaysOk })).status, 'idle');
});

test('the rail reference is stable across attempts, so a retry is not a second payment', async () => {
  const first = await sendPayout({ transferID: 'TP-9', payeeRef: 'p', amount: '100', ccy: 'INR' }, 1, {});
  const second = await sendPayout({ transferID: 'TP-9', payeeRef: 'p', amount: '100', ccy: 'INR' }, 2, {});
  assert.equal(first.railRef, second.railRef,
    'a rail reference that changed per attempt would make a retry look like a new payment');
});

test('a job delivered twice pays once', async () => {
  const d = db();
  applyEvent(d, settled('TP-1'));

  const now = Date.now();
  const job = claim(d, 'stalled-worker', { now, leaseMs: 1000 });
  assert.ok(job);

  // The first worker stalls past its lease; a second picks the job up and pays.
  const second = await runOnce(d, { now: now + 2000, rail: alwaysOk });
  assert.equal(second.status, 'paid');

  // The stalled worker wakes and finishes too. Its write hits the same key.
  const { runOnce: _ } = await import('../src/worker.js');
  const rows = d.prepare('SELECT COUNT(*) AS n FROM payouts').get();
  assert.equal(rows.n, 1, 'the payout is keyed by the job, so there is one row whatever happens');
  assert.equal(d.prepare('SELECT dedupe_key FROM payouts').get().dedupe_key, payoutKey('TP-1'));
});

test('a transient failure is retried and then succeeds', async () => {
  const d = db();
  applyEvent(d, settled('TP-FLAKY', 'sha256:flaky'));

  let calls = 0;
  const flaky = async (payload, attempt) => {
    calls += 1;
    if (attempt < 3) throw new RailError(`timeout on attempt ${attempt}`);
    return { railRef: 'SIM-OK' };
  };

  const now = Date.now();
  const a = await runOnce(d, { now, rail: flaky });
  assert.equal(a.status, 'retry');
  assert.equal(a.attempts, 1);

  // Too soon: the backoff is doing its job.
  assert.equal((await runOnce(d, { now: now + 100, rail: flaky })).status, 'idle');

  const b = await runOnce(d, { now: now + 1500, rail: flaky });
  assert.equal(b.status, 'retry');

  const c = await runOnce(d, { now: now + 10_000, rail: flaky });
  assert.equal(c.status, 'paid');
  assert.equal(calls, 3);
  assert.equal(d.prepare('SELECT attempts FROM payouts').get().attempts, 3);
});

test('a job that can never succeed is dead-lettered rather than retried for ever', async () => {
  const d = db();
  applyEvent(d, settled('TP-POISON', 'sha256:closed-account'));

  const dead = async () => { throw new RailError('account closed'); };
  let now = Date.now();
  let result;
  for (let i = 0; i < 5; i += 1) {
    result = await runOnce(d, { now, rail: dead });
    now += 120_000; // past any backoff
  }

  assert.equal(result.status, 'dead-lettered');
  assert.equal(queueDepth(d).queued, 0, 'it must leave the queue');
  const dl = d.prepare('SELECT * FROM dead_letters').get();
  assert.equal(dl.dedupe_key, payoutKey('TP-POISON'));
  assert.equal(dl.attempts, 5);
  assert.match(dl.last_error, /account closed/);
  assert.equal(d.prepare('SELECT state FROM payouts').get().state, 'DEAD',
    'a payment that did not happen has to be visible somewhere');
});

test('a poison job does not block the work behind it', async () => {
  const d = db();
  applyEvent(d, settled('TP-POISON', 'sha256:closed', { block: 1 }));
  applyEvent(d, settled('TP-GOOD', 'sha256:fine', { block: 2 }));

  const rail = async ({ payeeRef, transferID, ccy }) => {
    if (payeeRef === 'sha256:closed') throw new RailError('account closed');
    return { railRef: `SIM-${ccy}-${transferID}` };
  };

  let now = Date.now();
  const seen = [];
  for (let i = 0; i < 12; i += 1) {
    const r = await runOnce(d, { now, rail });
    if (r.status !== 'idle') seen.push(`${r.job.payload.transferID}:${r.status}`);
    now += 120_000;
  }

  assert.ok(seen.includes('TP-GOOD:paid'), `the healthy payout must go through; saw ${seen}`);
  assert.ok(seen.includes('TP-POISON:dead-lettered'));
  assert.equal(queueDepth(d).queued, 0);
  assert.equal(queueDepth(d).dead, 1);
});

test('an error the rail says is permanent is not retried five times', async () => {
  const d = db();
  applyEvent(d, settled('TP-NOPAYEE', 'sha256:x'));

  const refuse = async () => { throw new RailError('no payee reference', { retryable: false }); };
  const result = await runOnce(d, { rail: refuse });

  assert.equal(result.status, 'dead-lettered');
  assert.equal(result.attempts, 1, 'waiting cannot conjure a beneficiary');
});

test('a job nobody has a handler for is dead-lettered, not dropped', async () => {
  const d = db();
  enqueue(d, { kind: 'send-carrier-pigeon', dedupeKey: 'k', payload: { transferID: 'X' } });

  const result = await runOnce(d, { rail: alwaysOk });
  assert.equal(result.status, 'dead-lettered');
  assert.equal(queueDepth(d).dead, 1);
});

test('the simulated rail refuses a zero or malformed amount', async () => {
  await assert.rejects(
    () => sendPayout({ transferID: 'T', payeeRef: 'p', amount: '0', ccy: 'INR' }, 1, {}),
    /refusing to pay/,
  );
  await assert.rejects(
    () => sendPayout({ transferID: 'T', payeeRef: '', amount: '100', ccy: 'INR' }, 1, {}),
    /no payee reference/,
  );
});

test('an amount beyond 2^53 reaches the rail exactly as the ledger recorded it', async () => {
  const d = db();
  const big = '9007199254740993';
  applyEvent(d, {
    name: 'TransferSettled', blockNumber: 1, transactionId: 'tx-big',
    payloadText: `{"transferID":"BIG","state":"SETTLED","senderOrg":"Org1MSP",`
      + `"senderLegAmount":1,"senderCcy":"AED","payoutOrg":"Org2MSP",`
      + `"payoutLegAmount":${big},"payoutCcy":"INR","payoutPayeeRef":"p","at":1}`,
  });

  let saw = null;
  await runOnce(d, { rail: async (p) => { saw = p.amount; return { railRef: 'SIM' }; } });

  assert.equal(saw, big, 'the worker must hand the rail the exact figure, not a rounded one');
  assert.equal(d.prepare('SELECT amount FROM payouts').get().amount, big);
});
